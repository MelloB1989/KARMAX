package main

import (
	"flag"
	"strings"
)

// followupCmd is `followup <task> <text…>` and `cancel <task>`: each one
// POST to the task's route, with the sender and nothing else decided here.
func followupCmd(cmd string, args []string) error {
	from := sessionName()
	var reason string
	_, pos, err := flags(cmd, args, func(fs *flag.FlagSet) {
		fs.StringVar(&from, "from", from, "")
		if cmd == "cancel" {
			fs.StringVar(&reason, "reason", "", "")
		}
	})
	if err != nil {
		return err
	}
	if cmd == "cancel" {
		if err := need(pos, 1, "cancel <task> [--from NAME] [--reason TEXT]"); err != nil {
			return err
		}
		return post("/v1/tasks/"+pos[0]+"/cancel", map[string]string{"from": from, "reason": reason})
	}
	if err := need(pos, 2, `followup <task> "<text>" [--from NAME]`); err != nil {
		return err
	}
	return post("/v1/tasks/"+pos[0]+"/followup", map[string]string{"from": from, "text": strings.Join(pos[1:], " ")})
}
