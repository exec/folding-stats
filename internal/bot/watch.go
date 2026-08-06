package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The watcher is the only part of this bot that acts without being asked.
//
// It is driven by the data rather than by a clock: upstream publishes roughly hourly
// and drifts about ten seconds later each time, so anything on a fixed schedule would
// either evaluate stale figures or evaluate the same snapshot twice. Instead it polls
// the cheap /v1/status route and does its work when the snapshot time changes, which
// makes "once per publish" exact rather than approximate.
//
// Evaluating twice on one snapshot is the failure that matters: every rule here fires
// on a transition, and a repeated evaluation of the same reading would re-announce
// whatever the first one announced.
const watchPoll = 60 * time.Second

func (b *Bot) watch(ctx context.Context) {
	if b.alerts == nil {
		return
	}
	t := time.NewTicker(watchPoll)
	defer t.Stop()

	// The snapshot in force at start-up is not evaluated. A restart is not a publish,
	// and the alternative — treating "I have not seen this snapshot" as "this snapshot
	// is new" — turns every deploy into a round of notifications.
	var last time.Time
	if env, err := b.api.GetEnvelope(ctx, "/v1/status"); err == nil {
		last = env.Snapshot.At
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		env, err := b.api.GetEnvelope(ctx, "/v1/status")
		if err != nil {
			// Upstream being briefly unreachable is ordinary; the next tick retries and
			// nothing is lost, because the trigger is a snapshot time and not this poll.
			b.log.Debug("watch: status unavailable", "err", err)
			continue
		}
		if !env.Snapshot.At.After(last) {
			continue
		}
		last = env.Snapshot.At
		b.runAlerts(ctx, env.Snapshot)
	}
}

// runAlerts evaluates every alert against one snapshot.
func (b *Bot) runAlerts(ctx context.Context, snap Snapshot) {
	all := b.alerts.All()
	if len(all) == 0 {
		return
	}
	b.alertMu.Lock()
	defer b.alertMu.Unlock()

	start := time.Now()
	var fired, failed int

	// One reading per distinct target, however many alerts watch it. The client caches
	// by snapshot anyway, so this is belt and braces — but it also keeps the log honest
	// about how much work a hundred alerts actually is.
	readings := map[string]entity{}
	missing := map[string]bool{}

	for _, a := range all {
		key := a.Kind + "\x00" + a.Target
		e, ok := readings[key]
		if !ok {
			if missing[key] {
				continue
			}
			var err error
			e, err = b.reading(ctx, a.Kind, a.Target)
			if err != nil {
				// A target that has vanished upstream is not a delivery failure and must
				// not disable the alert: donors drop out of a feed and come back.
				missing[key] = true
				b.log.Debug("watch: target unavailable", "kind", a.Kind, "target", a.Target, "err", err)
				continue
			}
			readings[key] = e
		}

		fire, headline, detail, next := evaluate(a, e, snap.ServerTime)
		a.Seen = next
		if !fire {
			continue
		}
		if err := b.deliver(a, AlertEmbed(a, headline, detail, snap)); err != nil {
			failed++
			continue
		}
		fired++
	}

	if err := b.alerts.Save(); err != nil {
		b.log.Error("watch: saving alert state", "err", err)
	}
	if fired > 0 || failed > 0 {
		b.log.Info("alerts evaluated", "alerts", len(all), "targets", len(readings),
			"fired", fired, "failed", failed, "took", time.Since(start).Round(time.Millisecond))
	}
}

// reading fetches one target as the shape the rules read.
func (b *Bot) reading(ctx context.Context, kind, target string) (entity, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if kind == "team" {
		id, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			return entity{}, fmt.Errorf("team target %q is not an id", target)
		}
		t, _, err := b.api.Team(ctx, id)
		if err != nil {
			return entity{}, err
		}
		return entity{
			Name: t.Name, Rank: t.Rank, PointsTotal: t.PointsTotal,
			Last24h: t.PointsLast24h, PerDay: t.PointsPerDay, WUs: t.WUsTotal,
		}, nil
	}
	d, _, err := b.api.Donor(ctx, target)
	if err != nil {
		return entity{}, err
	}
	return entity{
		Name: d.Name, Rank: d.Rank, PointsTotal: d.PointsTotal,
		Last24h: d.PointsLast24h, PerDay: d.PointsPerDay, WUs: d.WUsTotal,
	}, nil
}

// deliver posts an alert, and gives up on a channel that keeps refusing it.
//
// A bot that cannot post has no way to say so — the one place it would report the
// problem is the channel it cannot reach. So a channel that is deleted, or that the
// bot has been removed from, would otherwise generate a failed request every hour
// forever, for an audience of nobody. Permanent refusals disable the alert outright;
// anything else gets three strikes, so a transient outage does not lose a subscription.
func (b *Bot) deliver(a *Alert, e *discordgo.MessageEmbed) error {
	if b.session == nil {
		return errors.New("no session")
	}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{e}}
	if a.Tag != "" {
		msg.Content = a.Tag
	}
	_, err := b.session.ChannelMessageSendComplex(a.ChannelID, msg)
	if err == nil {
		a.Failures = 0
		return nil
	}

	var rest *discordgo.RESTError
	permanent := false
	if errors.As(err, &rest) && rest.Message != nil {
		switch rest.Message.Code {
		case 10003, // unknown channel
			50001, // missing access
			50013: // missing permissions
			permanent = true
		}
	}
	a.Failures++
	if permanent || a.Failures >= 3 {
		b.log.Warn("alert removed: cannot post",
			"id", a.ID, "channel", a.ChannelID, "target", a.Label, "err", err)
		if _, rmErr := b.alerts.Remove(a.ID); rmErr != nil {
			b.log.Error("removing undeliverable alert", "id", a.ID, "err", rmErr)
		}
		return err
	}
	b.log.Warn("alert delivery failed", "id", a.ID, "attempt", a.Failures, "err", err)
	return err
}

// AlertEmbed renders one firing.
//
// Deliberately plainer than the lookup embeds: this arrives unprompted, so it says the
// one thing that changed and links to the page rather than reprinting a stat block
// nobody asked for. The footer still carries the data's age, because an alert is the
// most likely message here to be read hours after it was sent.
func AlertEmbed(a *Alert, headline, detail string, s Snapshot) *discordgo.MessageEmbed {
	colour := colourNormal
	switch a.Type {
	case AlertIdle:
		colour = colourWarn
	case AlertMilestone, AlertResumed:
		colour = colourGood
	}
	return &discordgo.MessageEmbed{
		Title:       headline,
		URL:         a.TargetURL(),
		Description: detail,
		Color:       colour,
		Footer:      footer(s),
	}
}
