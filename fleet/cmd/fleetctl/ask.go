package main

import (
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/daemon"
)

// askCmd is `ask "<question>"`, for an agent: file a question under its
// current task to whoever assigned it, and block until the answer arrives.
// The answer is the output, so the agent goes on in the same turn.
func askCmd(args []string) error {
	from, timeout := sessionName(), daemon.DefaultAskTimeout
	_, pos, err := flags("ask", args, func(fs *flag.FlagSet) {
		fs.StringVar(&from, "from", from, "")
		fs.DurationVar(&timeout, "timeout", timeout, "")
	})
	if err != nil {
		return err
	}
	if err := need(pos, 1, `ask "<question>" [--timeout 30m]`); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	c.timeout = timeout + 2*time.Minute
	var rep daemon.AskReply
	if err := c.do("POST", "/v1/ask", map[string]string{"from": from, "text": strings.Join(pos, " "), "timeout": timeout.String()}, &rep); err != nil {
		if retryable(err) {
			// Not sent again: the question may be filed, and then an answer
			// reaches you as a message.
			return fmt.Errorf("%w (the question may still be filed; if it is answered you get the answer as a message)", err)
		}
		return err
	}
	if rep.TimedOut {
		fmt.Printf("no answer to your question in %s; it stays filed, and an answer after this comes as a message. Go on without it or report that you are blocked.\n", timeout)
		return exitCode{4}
	}
	fmt.Println(rep.Answer)
	return nil
}

// answerCmd is `answer <task> "<text>"`: answer the question the task's agent
// is blocked on. It reaches its ask as the output; with nothing blocked it is
// a follow-up.
func answerCmd(args []string) error {
	from := sessionName()
	_, pos, err := flags("answer", args, func(fs *flag.FlagSet) { fs.StringVar(&from, "from", from, "") })
	if err != nil {
		return err
	}
	if err := need(pos, 2, `answer <task> "<text>" [--from NAME]`); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var rep struct {
		Answered bool   `json:"answered"`
		Queued   bool   `json:"queued"`
		Why      string `json:"why"`
	}
	if err := c.do("POST", "/v1/tasks/"+url.PathEscape(pos[0])+"/answer", map[string]string{"from": from, "text": strings.Join(pos[1:], " ")}, &rep); err != nil {
		return err
	}
	switch {
	case rep.Queued:
		fmt.Println(rep.Why)
	case rep.Answered:
		fmt.Println("answered: the agent's ask returned it")
	default:
		fmt.Println("nothing was waiting on a question; sent as a follow-up")
	}
	return nil
}
