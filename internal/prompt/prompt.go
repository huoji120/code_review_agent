package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"code-review-agent/internal/config"
)

type Prompts struct {
	System     string
	PlanSystem string
	Compress   string
	Skills     []Skill
	loaded     []string
	Templates  map[string]string
}

type Skill struct {
	Name    string
	Content string
}

func Load(cfg config.PromptConfig) (Prompts, error) {
	system, err := os.ReadFile(cfg.System)
	if err != nil {
		return Prompts{}, err
	}
	planSystem, err := os.ReadFile(cfg.PlanSystem)
	if err != nil {
		return Prompts{}, err
	}
	compress, err := os.ReadFile(cfg.Compress)
	if err != nil {
		return Prompts{}, err
	}
	skills, err := loadSkills(cfg.SkillsDir)
	if err != nil {
		return Prompts{}, err
	}
	templates, err := loadTemplates(cfg.TemplatesDir)
	if err != nil {
		return Prompts{}, err
	}
	return Prompts{System: string(system), PlanSystem: string(planSystem), Compress: string(compress), Skills: skills, Templates: templates}, nil
}

func (p Prompts) RenderTemplate(name string, vars map[string]string) string {
	template := p.Templates[name]
	for key, value := range vars {
		template = strings.ReplaceAll(template, "!{"+key+"}", value)
	}
	return template
}

func (p Prompts) SystemWithSkills() string {
	return p.systemWithSkills(p.System)
}

func (p Prompts) PlanSystemWithSkills() string {
	return p.systemWithSkills(p.PlanSystem)
}

func (p Prompts) systemWithSkills(system string) string {
	var b strings.Builder
	b.WriteString(system)
	if len(p.Skills) > 0 {
		b.WriteString("\n\n# Available Skills\n")
		b.WriteString("如需使用某个 skill，必须先调用 `load_skill`。你可以按需加载多个不同 skill 并组合使用；加载后不要重复加载同一个 skill。可用 skills：\n")
		for _, skill := range p.Skills {
			b.WriteString("- ")
			b.WriteString(skill.Name)
			if summary := summarizeSkill(skill.Content); summary != "" {
				b.WriteString("：")
				b.WriteString(summary)
			}
			b.WriteString("\n")
		}
	}
	loaded := p.LoadedSkills()
	if len(loaded) == 0 {
		return b.String()
	}
	b.WriteString("\n\n# Loaded Skills\n")
	for _, skill := range loaded {
		b.WriteString("\n## Skill: ")
		b.WriteString(skill.Name)
		b.WriteString("\n")
		b.WriteString(skill.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func (p *Prompts) LoadSkill(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, loaded := range p.loaded {
		if loaded == name {
			return false
		}
	}
	for _, skill := range p.Skills {
		if skill.Name == name {
			p.loaded = append(p.loaded, name)
			return true
		}
	}
	return false
}

func (p *Prompts) SetLoadedSkills(names []string) {
	p.loaded = nil
	for _, name := range names {
		p.LoadSkill(name)
	}
}

func (p Prompts) LoadedSkills() []Skill {
	var loaded []Skill
	for _, name := range p.loaded {
		for _, skill := range p.Skills {
			if skill.Name == name {
				loaded = append(loaded, skill)
				break
			}
		}
	}
	return loaded
}

func (p Prompts) LoadedSkillNames() []string {
	out := make([]string, len(p.loaded))
	copy(out, p.loaded)
	return out
}

func (p Prompts) HasSkill(name string) bool {
	for _, skill := range p.Skills {
		if skill.Name == name {
			return true
		}
	}
	return false
}

func summarizeSkill(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		return line
	}
	return ""
}

func loadSkills(root string) ([]Skill, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load skill %s: %w", entry.Name(), err)
		}
		skills = append(skills, Skill{Name: entry.Name(), Content: string(data)})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

func loadTemplates(root string) (map[string]string, error) {
	templates := map[string]string{}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return templates, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		templates[name] = string(data)
	}
	return templates, nil
}
