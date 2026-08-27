package keka

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Read-only, deliberately. Approving leave or editing a record is a different
// risk class and belongs behind a proposal an operator reads, not behind a tool
// the model can reach for on its own.

type employee struct {
	ID             string `json:"id"`
	EmployeeNumber string `json:"employeeNumber"`
	FirstName      string `json:"firstName"`
	LastName       string `json:"lastName"`
	DisplayName    string `json:"displayName"`
	Email          string `json:"email"`
	JobTitle       struct {
		Title string `json:"title"`
	} `json:"jobTitle"`
	Department struct {
		Name string `json:"name"`
	} `json:"department"`
	ReportingManager struct {
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	} `json:"reportingManager"`
	EmploymentStatus string `json:"employmentStatus"`
}

func (e employee) flatten() map[string]any {
	name := strings.TrimSpace(e.DisplayName)
	if name == "" {
		name = strings.TrimSpace(e.FirstName + " " + e.LastName)
	}
	out := map[string]any{"id": e.ID, "name": name}
	for k, v := range map[string]string{
		"employee_number": e.EmployeeNumber,
		"email":           e.Email,
		"title":           e.JobTitle.Title,
		"department":      e.Department.Name,
		"manager":         e.ReportingManager.DisplayName,
		"status":          e.EmploymentStatus,
	} {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

func (c *Connector) Tools() []connectorkit.Tool {
	return []connectorkit.Tool{
		{
			Name: "keka.employees",
			Description: "Search people in Keka by name or email, returning their title, department " +
				"and reporting manager. Use to answer who someone is or who they report to.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"Part of a name or email. Empty lists everyone."},
					"limit":{"type":"integer","description":"default 25, max 100"}
				}
			}`),
			Call: employees,
		},
		{
			Name: "keka.leave",
			Description: "Who is on leave, and when. Use to answer whether someone is available on " +
				"a given day, or who is out this week.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"from":{"type":"string","description":"Start date, YYYY-MM-DD. Defaults to today."},
					"to":{"type":"string","description":"End date, YYYY-MM-DD. Defaults to 14 days after 'from'."},
					"limit":{"type":"integer","description":"default 50, max 200"}
				}
			}`),
			Call: leave,
		},
		{
			Name:        "keka.departments",
			Description: "List departments in Keka, for when a question is about a team rather than a person.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			Call:        departments,
		},
	}
}

func employees(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	limit := intArg(in, "limit", 25)
	if limit > 100 {
		limit = 100
	}
	q := url.Values{"pageSize": {fmt.Sprint(limit)}}
	if s := strArg(in, "query"); s != "" {
		q.Set("search", s)
	}

	var res struct {
		Data []employee `json:"data"`
	}
	if err := call(ctx, cr, "/hris/employees", q, &res); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(res.Data))
	for _, e := range res.Data {
		out = append(out, e.flatten())
	}
	return map[string]any{"count": len(out), "employees": out}, nil
}

func leave(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	from := strArg(in, "from")
	if from == "" {
		from = time.Now().UTC().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", from); err != nil {
		return nil, fmt.Errorf("keka: 'from' must be YYYY-MM-DD")
	}
	to := strArg(in, "to")
	if to == "" {
		start, _ := time.Parse("2006-01-02", from)
		to = start.AddDate(0, 0, 14).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", to); err != nil {
		return nil, fmt.Errorf("keka: 'to' must be YYYY-MM-DD")
	}
	limit := intArg(in, "limit", 50)
	if limit > 200 {
		limit = 200
	}

	var res struct {
		Data []struct {
			Employee  employee `json:"employee"`
			LeaveType struct {
				Name string `json:"name"`
			} `json:"leaveType"`
			FromDate string `json:"fromDate"`
			ToDate   string `json:"toDate"`
			Status   string `json:"leaveStatus"`
		} `json:"data"`
	}
	if err := call(ctx, cr, "/leave/leaverequests", url.Values{
		"from": {from}, "to": {to}, "pageSize": {fmt.Sprint(limit)},
	}, &res); err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(res.Data))
	for _, r := range res.Data {
		row := map[string]any{
			"person": r.Employee.flatten()["name"],
			"from":   dateOnly(r.FromDate),
			"to":     dateOnly(r.ToDate),
		}
		if r.LeaveType.Name != "" {
			row["type"] = r.LeaveType.Name
		}
		// Status matters: a PENDING request is not time off yet, and answering
		// "she's out Thursday" from an unapproved request would be wrong.
		if r.Status != "" {
			row["status"] = r.Status
		}
		out = append(out, row)
	}
	return map[string]any{"count": len(out), "from": from, "to": to, "leave": out}, nil
}

func departments(ctx context.Context, cr connectorkit.Credentials, _ map[string]any) (any, error) {
	var res struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := call(ctx, cr, "/hris/departments", url.Values{"pageSize": {"200"}}, &res); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(res.Data))
	for _, d := range res.Data {
		out = append(out, map[string]any{"id": d.ID, "name": d.Name})
	}
	return map[string]any{"count": len(out), "departments": out}, nil
}

// dateOnly trims a Keka timestamp to its date.
//
// Keka returns full timestamps for what are calendar days, and "2026-08-27T00:00:00"
// shown to someone asking who is off on Thursday is noise at best and a
// timezone argument at worst.
func dateOnly(s string) string {
	if i := strings.IndexByte(s, 'T'); i > 0 {
		return s[:i]
	}
	return s
}

func strArg(in map[string]any, key string) string {
	if v, ok := in[key].(string); ok {
		return v
	}
	return ""
}

func intArg(in map[string]any, key string, def int) int {
	switch n := in[key].(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return def
}
