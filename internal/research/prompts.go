package research

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

var promptTemplates = func() map[string]*template.Template {
	names := []string{"classify", "plan", "notes", "reflect", "write"}
	out := make(map[string]*template.Template, len(names))
	for _, n := range names {
		out[n] = template.Must(template.ParseFS(promptFS, "prompts/"+n+".tmpl"))
	}
	return out
}()

func renderPrompt(name string, data any) string {
	t := promptTemplates[name]
	if t == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return ""
	}
	return buf.String()
}

// MiniTier and ProTier mirror the documented budgets; deadlines and hard limits
// are overridden from env by the manager.
func MiniTier() Tier {
	return Tier{Name: "mini", Rounds: 1, Queries: 4, Pages: 8, Deadline: 0,
		MaxLLM: 12, MaxTokens: 40000, ReportMin: 400, ReportMax: 800, SearchDepth: "advanced"}
}

// ProTier is the deeper profile.
func ProTier() Tier {
	return Tier{Name: "pro", Rounds: 3, Queries: 8, Pages: 20, Deadline: 0,
		MaxLLM: 40, MaxTokens: 120000, ReportMin: 800, ReportMax: 2000, SearchDepth: "advanced"}
}
