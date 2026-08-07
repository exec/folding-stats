package bot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The gateway can wedge in a way nothing notices.
//
// This happened: the bot sat "active (running)" for two and a half hours, answering
// nothing. The websocket to Discord had 88 bytes stuck in the socket's send queue —
// a heartbeat written into a connection whose path had gone away. The network
// recovered; that one flow did not.
//
// discordgo has the right idea and reconnects after five missed heartbeat ACKs, but
// the check sits on the line *after* the write:
//
//	err = wsConn.WriteJSON(heartbeatOp{1, sequence})
//	if err != nil || time.Now().UTC().Sub(last) > (interval*FailedHeartbeatAcks) { reconnect }
//
// gorilla/websocket sets no write deadline, so on a half-open connection that write
// blocks forever and the reconnect check is never reached. The heartbeat goroutine
// parks, the process stays healthy by every measure systemd can see, and Restart=always
// never fires because nothing ever exits.
//
// Nothing in-process can rescue it: the stuck write holds the session's write mutex,
// so Close and reconnect would block behind it too. The only reliable escape is to stop
// being this process. So the watchdog exits, and systemd — which already has
// Restart=always with a 15s delay — starts a new one with a fresh connection.

// gatewaySilence is how long without a heartbeat ACK counts as dead.
//
// Discord's interval is about 41s and discordgo tolerates five misses, so it should
// have recovered on its own inside four minutes. This is deliberately well past that:
// it fires only when discordgo's own recovery is the thing that is stuck, which is the
// case that produced hours of silence.
const gatewaySilence = 6 * time.Minute

// gatewayCheck is how often the connection is examined.
const gatewayCheck = 30 * time.Second

// watchGateway restarts the process when the connection has gone quiet.
//
// The interval and the exit are parameters so a test can drive both: a watchdog whose
// only observable behaviour is terminating the process is otherwise untestable, and
// this one guards a failure that took hours to notice by hand.
func (b *Bot) watchGateway(ctx context.Context, every time.Duration, exit func(int)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		age, ok := b.ackAge()
		if !ok {
			// No ACK recorded yet: still connecting, or between reconnects. Not
			// evidence of anything, and exiting here would fight a normal start-up.
			continue
		}
		if age > gatewaySilence {
			b.log.Error("gateway is silent; exiting so systemd reconnects",
				"since_last_ack", age.Round(time.Second), "threshold", gatewaySilence)
			exit(1)
			return
		}
	}
}

// ackAge reports how long since Discord last acknowledged a heartbeat.
func (b *Bot) ackAge() (time.Duration, bool) {
	if b.session == nil {
		return 0, false
	}
	b.session.RLock()
	last := b.session.LastHeartbeatAck
	b.session.RUnlock()
	if last.IsZero() {
		return 0, false
	}
	return time.Since(last), true
}

// routeDiscordLogs sends the library's own diagnostics to the same place as ours.
//
// The outage above left no trace at all, because discordgo's logger defaults to
// discarding everything below LogError and writes to the standard logger rather than
// anywhere structured. Reconnects, resumes and gateway errors are exactly the record
// needed to tell "Discord had a bad night" from "this bot has a bug".
func routeDiscordLogs(log *slog.Logger) {
	discordgo.Logger = func(msgL, _ int, format string, a ...any) {
		lvl := slog.LevelWarn
		switch msgL {
		case discordgo.LogError:
			lvl = slog.LevelError
		case discordgo.LogInformational:
			lvl = slog.LevelInfo
		case discordgo.LogDebug:
			lvl = slog.LevelDebug
		}
		log.Log(context.Background(), lvl, "gateway", "msg", fmt.Sprintf(format, a...))
	}
}
