package bot

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func quietBot(s *discordgo.Session) *Bot {
	return &Bot{
		session: s,
		log:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
	}
}

// TestAckAgeIgnoresAConnectionThatHasNotAckedYet keeps start-up out of the watchdog's
// way: a session with no ACK recorded is connecting, not dead, and treating the two the
// same would make the bot exit in a loop it could never start out of.
func TestAckAgeIgnoresAConnectionThatHasNotAckedYet(t *testing.T) {
	if _, ok := quietBot(nil).ackAge(); ok {
		t.Error("reported an age with no session")
	}
	if _, ok := quietBot(&discordgo.Session{}).ackAge(); ok {
		t.Error("reported an age before the first heartbeat ACK")
	}
	s := &discordgo.Session{}
	s.LastHeartbeatAck = time.Now().Add(-90 * time.Second)
	age, ok := quietBot(s).ackAge()
	if !ok {
		t.Fatal("no age from an acked session")
	}
	if age < 80*time.Second || age > 100*time.Second {
		t.Errorf("age = %s, want about 90s", age)
	}
}

// TestWatchGatewayExitsOnSilence is the whole point: the failure it covers left the
// process running and useless for two and a half hours, with Restart=always set and
// nothing to trigger it.
func TestWatchGatewayExitsOnSilence(t *testing.T) {
	s := &discordgo.Session{}
	s.LastHeartbeatAck = time.Now().Add(-2 * gatewaySilence)
	b := quietBot(s)

	got := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go b.watchGateway(ctx, 20*time.Millisecond, func(code int) { got <- code })

	select {
	case code := <-got:
		if code == 0 {
			t.Errorf("exited %d; a watchdog exit must be non-zero or systemd may treat it as a clean stop", code)
		}
	case <-ctx.Done():
		t.Fatal("a silent gateway did not trigger an exit")
	}
}

// And it must not fire on a connection that is merely idle between heartbeats, or the
// bot would restart every few minutes and lose its gateway session each time.
func TestWatchGatewayLeavesAHealthyConnectionAlone(t *testing.T) {
	s := &discordgo.Session{}
	s.LastHeartbeatAck = time.Now()
	b := quietBot(s)

	got := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go b.watchGateway(ctx, 20*time.Millisecond, func(code int) { got <- code })

	select {
	case code := <-got:
		t.Fatalf("exited %d on a healthy connection", code)
	case <-ctx.Done():
	}
}

// The threshold has to sit clear of discordgo's own recovery, or the watchdog fires
// during a reconnect the library was going to handle and turns a blip into a restart.
func TestGatewaySilenceIsPastDiscordgosOwnRecovery(t *testing.T) {
	// Discord's heartbeat interval is roughly 41s and discordgo tolerates five misses.
	const libraryRecovery = 5 * 45 * time.Second
	if gatewaySilence <= libraryRecovery {
		t.Errorf("gatewaySilence is %s, inside discordgo's own %s recovery window",
			gatewaySilence, libraryRecovery)
	}
}
