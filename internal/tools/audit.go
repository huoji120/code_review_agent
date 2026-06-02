package tools

import "encoding/json"

type Finding struct {
	ID             int    `json:"id"`
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Path           string `json:"path"`
	Line           int    `json:"line,omitempty"`
	Evidence       string `json:"evidence"`
	Impact         string `json:"impact"`
	Recommendation string `json:"recommendation"`
	CWE            string `json:"cwe,omitempty"`
}

type reportFindingArgs struct {
	Severity       string `json:"severity"`
	Title          string `json:"title"`
	Path           string `json:"path"`
	Line           int    `json:"line"`
	Evidence       string `json:"evidence"`
	Impact         string `json:"impact"`
	Recommendation string `json:"recommendation"`
	CWE            string `json:"cwe"`
}

type endAuditArgs struct {
	Summary   string `json:"summary"`
	NextSteps string `json:"next_steps"`
}

type auditSummary struct {
	Summary   string    `json:"summary"`
	NextSteps string    `json:"next_steps,omitempty"`
	Findings  []Finding `json:"findings"`
}

func (r *Registry) reportFinding(raw json.RawMessage) Result {
	args, err := decodeArgs[reportFindingArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Title == "" {
		return Result{OK: false, Error: "title is required"}
	}
	if args.Path == "" {
		return Result{OK: false, Error: "path is required"}
	}
	if args.Evidence == "" {
		return Result{OK: false, Error: "evidence is required"}
	}
	if args.Severity == "" {
		args.Severity = "medium"
	}
	finding := Finding{
		ID:             len(r.findings) + 1,
		Severity:       args.Severity,
		Title:          args.Title,
		Path:           args.Path,
		Line:           args.Line,
		Evidence:       args.Evidence,
		Impact:         args.Impact,
		Recommendation: args.Recommendation,
		CWE:            args.CWE,
	}
	r.findings = append(r.findings, finding)
	return Result{OK: true, Data: finding, Message: "finding recorded"}
}

func (r *Registry) endAudit(raw json.RawMessage) Result {
	args, err := decodeArgs[endAuditArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Summary == "" {
		args.Summary = "Audit completed."
	}
	r.audit = AuditState{Ended: true, Summary: args.Summary, NextSteps: args.NextSteps}
	findings := make([]Finding, len(r.findings))
	copy(findings, r.findings)
	return Result{OK: true, Data: auditSummary{Summary: args.Summary, NextSteps: args.NextSteps, Findings: findings}, Message: "audit ended"}
}

func (r *Registry) Findings() []Finding {
	out := make([]Finding, len(r.findings))
	copy(out, r.findings)
	return out
}
