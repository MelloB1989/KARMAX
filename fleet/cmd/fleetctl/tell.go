package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// tellBackoff is the pauses between tries of a tell fleetd did not answer.
var tellBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// outboxDir is where a fleet container queues the messages fleetd could not
// take: the slot's state directory's outbox (FLEET_STATE, which agent-run
// sets for each slot), else $FLEET_WORK/.fleet/outbox. Outside a fleet
// container there is none.
func outboxDir() string {
	if os.Getenv("FLEET_AGENT") == "" {
		return ""
	}
	if s := os.Getenv("FLEET_STATE"); s != "" {
		return filepath.Join(s, "outbox")
	}
	w := os.Getenv("FLEET_WORK")
	if w == "" {
		w = "/work"
	}
	return filepath.Join(w, ".fleet", "outbox")
}

// outboxMsg is one queued message, one line of JSON (fleetd reads the outbox
// in its snapshot of the container).
type outboxMsg struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// queuedLocally is what tell says of a message it left in the outbox.
const queuedLocally = "queued: fleetd is not answering; it delivers this from your outbox when it is back. Do not send it again."

// sendReply is fleetd's answer to a message: queued when it took the message
// but its receiver cannot have it yet (why says so).
type sendReply struct {
	Queued bool   `json:"queued"`
	Why    string `json:"why"`
}

// tell sends a message through fleetd and says how it went: "ok", fleetd's
// word that it holds the message for a receiver it cannot reach, or that it
// was queued here. When fleetd does not answer (it is restarting; its host is
// rebooting) after a few tries, and there is an outbox, the message is queued
// there and fleetd delivers it on its next look at the container: a report is
// never lost to a restart.
func tell(c *client, from, to, text, outbox string, sleep func(time.Duration)) (string, error) {
	body := map[string]string{"from": from, "to": to, "text": text}
	var rep sendReply
	err := c.do("POST", "/v1/tell", body, &rep)
	for _, d := range tellBackoff {
		if err == nil || !retryable(err) {
			break
		}
		sleep(d)
		err = c.do("POST", "/v1/tell", body, &rep)
	}
	if err == nil {
		if rep.Queued {
			return rep.Why, nil
		}
		return "ok", nil
	}
	if !retryable(err) || outbox == "" {
		return "", err
	}
	b, _ := json.Marshal(outboxMsg{From: from, To: to, Text: text, At: time.Now().UTC()})
	if qerr := writeAtomic(outbox, strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+strconv.Itoa(os.Getpid())+".json", b); qerr != nil {
		return "", fmt.Errorf("%w (and it could not be queued: %v)", err, qerr)
	}
	return queuedLocally, nil
}

// writeAtomic writes a file whole or not at all: fleetd never reads half.
func writeAtomic(dir, name string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
