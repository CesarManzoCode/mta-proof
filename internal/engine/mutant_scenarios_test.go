package engine

// Scenarios that isolate the behavior each mutant of contract 4.3 seeds.
// mutant_test.go checks the original engine on them; each mut_*_test.go,
// built only with its tag, checks that the mutant departs from it.

import (
	"context"
	"testing"
	"time"

	"github.com/CesarManzoCode/mta-proof/internal/clock"
	"github.com/CesarManzoCode/mta-proof/internal/store"
)

// backoffStarts are the attempt starts of one message that is always
// deferred, under testConfig's policy (60 s, x2, max 600 s, lifetime 3600 s).
func backoffStarts(t *testing.T) []int64 {
	e := newDeliveryEnv(t, testConfig())
	h := e.host("always-defer.test", "mx1.always-defer.test", "127.0.1.6")
	h.dataEnd = func([]byte) *Reply {
		return &Reply{Code: 451, Enhanced: "4.3.0", Text: "Requested action aborted: local error in processing"}
	}
	id := e.ingest("acme", "app@sender.test", "user@always-defer.test")
	e.run(48 * time.Hour)
	var starts []int64
	for _, ev := range e.events(id, KindAttemptStarted) {
		starts = append(starts, ev.AtMs)
	}
	return starts
}

// permanentRefusal is the final state and attempt count of one message
// whose RCPT is refused with a permanent 550 5.1.1.
func permanentRefusal(t *testing.T) (string, int) {
	e := newDeliveryEnv(t, testConfig())
	h := e.host("gmail-model.test", "mx1.gmail-model.test", "127.0.1.1")
	h.rcpt = func(string) *Reply {
		return &Reply{Code: 550, Enhanced: "5.1.1", Text: "The email account that you tried to reach does not exist."}
	}
	id := e.ingest("acme", "app@sender.test", "gone@gmail-model.test")
	e.run(48 * time.Hour)
	return e.state(id), len(e.events(id, KindAttemptStarted))
}

// silentCloses counts the silent closes of R-YAHOO that 20 messages meet
// on a receiver that closes at the sixth MAIL of a connection, with one
// connection at a time.
func silentCloses(t *testing.T) int {
	cfg := testConfig()
	cfg.Limits.Domain = map[string]Limit{"*": {Connections: 1}}
	e := newDeliveryEnv(t, cfg)
	h := e.host("yahoo-model.test", "mx1.yahoo-model.test", "127.0.1.3")
	h.silentAfter = 5
	ids := e.ingestN(20, "yahoo-model.test")
	e.run(0)
	e.allState(store.StateDelivered, ids...)
	n := 0
	for _, ev := range e.events("", KindDeferred) {
		if ev.RuleID == "yahoo.silent_close" {
			n++
		}
	}
	return n
}

// ackBeforeCommit reports whether Runner.Ingest returned while the group
// holding the new message was not yet committed, and whether the message
// was in engine.db at that moment. The ingest shares its group with an
// operation that blocks until the test releases it.
func ackBeforeCommit(t *testing.T) (returned, persisted bool) {
	path := t.TempDir() + "/engine.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	core, err := OpenCore(st, clock.Real{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(core, st, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	gateA, gateC := make(chan struct{}), make(chan struct{})
	started := make(chan struct{})
	go r.Do(ctx, func(*Core) any { close(started); <-gateA; return nil })
	<-started
	ingested := make(chan string, 1)
	go func() {
		s := &Submission{Tenant: "acme", From: "app@sender.test", To: "u@baseline.test", Subject: "Hello",
			Text: "body", MsgType: TypeTransactional}
		res, err := r.Ingest(ctx, s, "")
		if err != nil {
			t.Error(err)
		}
		ingested <- res.MsgID
	}()
	waitLen(t, r, 1)
	cDone := make(chan struct{})
	go func() { r.Do(ctx, func(*Core) any { <-gateC; return nil }); close(cDone) }()
	waitLen(t, r, 2)
	close(gateA) // the ingest and the blocking operation form the next group

	select {
	case id := <-ingested:
		returned = true
		var n int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE msg_id = ?", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		persisted = n == 1
	case <-time.After(500 * time.Millisecond):
	}
	close(gateC)
	<-cDone
	if !returned {
		<-ingested
	}
	return returned, persisted
}

func waitLen(t *testing.T, r *Runner, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(r.ops) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("%d operations queued, want %d", len(r.ops), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// fastDeliveries are the delivery instants of five messages to fast.test
// ingested after five to slow.test, which the engine limits to one attempt
// a minute.
func fastDeliveries(t *testing.T) []int64 {
	cfg := testConfig()
	cfg.Limits.Domain = map[string]Limit{"slow.test": {Rate: &Rate{Limit: 1, PerS: 60}}}
	e := newDeliveryEnv(t, cfg)
	e.host("slow.test", "mx1.slow.test", "127.0.1.60")
	e.host("fast.test", "mx1.fast.test", "127.0.1.61")
	e.ingestN(5, "slow.test")
	fast := e.ingestN(5, "fast.test")
	e.run(time.Hour)
	var at []int64
	for _, id := range fast {
		at = append(at, e.events(id, KindDelivered)[0].AtMs)
	}
	return at
}

// firstHourStarts counts the attempts in the first hour from one address
// whose plan allows 100 a day and 10 an hour, with 50 messages queued.
func firstHourStarts(t *testing.T) int {
	e := newDeliveryEnv(t, warmupConfig(t, PoolIP{IP: "127.0.2.1", Plan: plan([]int{100}, []int{10})}))
	e.host("baseline.test", "mx1.baseline.test", "127.0.1.5")
	e.ingestN(50, "baseline.test")
	e.runTo(hourMs - 1)
	return e.starts(hourMs)["127.0.2.1"][0]
}
