package skillspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// ParseFrontmatter parses a SKILL.md document's YAML frontmatter into a
// JSON-compatible map (preserving all author fields verbatim and keeping YAML
// timestamps as strings) along with the markdown body after the closing "---".
func ParseFrontmatter(data []byte) (map[string]any, string, error) {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return nil, "", fmt.Errorf("SKILL.md must begin with YAML frontmatter (---)")
	}
	rest := normalized[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	bodyOffset := 0
	if end >= 0 {
		bodyOffset = 4 + end + len("\n---\n")
	} else if bytes.HasSuffix(rest, []byte("\n---")) {
		end = len(rest) - len("\n---")
		bodyOffset = len(normalized)
	} else {
		return nil, "", fmt.Errorf("SKILL.md frontmatter has no closing --- delimiter")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(rest[:end], &doc); err != nil {
		return nil, "", fmt.Errorf("parse SKILL.md YAML frontmatter: %w", err)
	}
	preserveTimestampStrings(&doc)
	var fields map[string]any
	if err := doc.Decode(&fields); err != nil {
		return nil, "", fmt.Errorf("decode SKILL.md frontmatter: %w", err)
	}
	if fields == nil {
		return nil, "", fmt.Errorf("SKILL.md frontmatter is empty")
	}
	for k, v := range fields {
		nv, err := normalizeYAML(v)
		if err != nil {
			return nil, "", fmt.Errorf("frontmatter field %q: %w", k, err)
		}
		fields[k] = nv
	}
	body := ""
	if bodyOffset < len(normalized) {
		body = string(normalized[bodyOffset:])
	}
	return fields, body, nil
}

func preserveTimestampStrings(node *yaml.Node) {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!timestamp" {
		node.Tag = "!!str"
	}
	for _, child := range node.Content {
		preserveTimestampStrings(child)
	}
}

func normalizeYAML(value any) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		for k, item := range v {
			n, err := normalizeYAML(item)
			if err != nil {
				return nil, err
			}
			v[k] = n
		}
		return v, nil
	case map[any]any:
		res := make(map[string]any, len(v))
		for k, item := range v {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("mapping key must be a string")
			}
			n, err := normalizeYAML(item)
			if err != nil {
				return nil, err
			}
			res[ks] = n
		}
		return res, nil
	case []any:
		for i, item := range v {
			n, err := normalizeYAML(item)
			if err != nil {
				return nil, err
			}
			v[i] = n
		}
		return v, nil
	default:
		return value, nil
	}
}

// ValidateName checks that name conforms to the Agent Skills / SEP-2640 rule:
// 1 to 64 lowercase Unicode letters, digits, or non-consecutive hyphens,
// with no leading or trailing hyphen.
func ValidateName(name string) error {
	invalid := fmt.Errorf("skill name %q must be 1–64 lowercase letters, digits, or non-consecutive hyphens (no leading/trailing hyphen)", name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		return invalid
	}
	if name != strings.ToLower(name) {
		return invalid
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return invalid
	}
	for _, r := range name {
		if r != '-' && !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return invalid
		}
	}
	return nil
}

// NormalizeSkillName lowercases name and replaces whitespace/underscores with hyphens.
func NormalizeSkillName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsSpace(r) || r == '_' || r == '-' {
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// IsInternalSkill reports whether a skill's frontmatter marks it as internal
// (metadata.internal = true / "true" / "1"), matching npx skills behavior.
func IsInternalSkill(fm map[string]any) bool {
	meta, ok := fm["metadata"].(map[string]any)
	if !ok {
		return false
	}
	switch v := meta["internal"].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	case int:
		return v == 1
	default:
		return false
	}
}

// RewriteFrontmatterName updates the `name` field in SKILL.md YAML frontmatter
// when a skill is renamed via --rename (matching npx skills behavior).
func RewriteFrontmatterName(data []byte, newName string) ([]byte, error) {
	if err := ValidateName(newName); err != nil {
		return nil, err
	}
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return nil, fmt.Errorf("SKILL.md must begin with YAML frontmatter (---)")
	}
	rest := normalized[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	suffix := "\n---\n"
	if end < 0 && bytes.HasSuffix(rest, []byte("\n---")) {
		end = len(rest) - len("\n---")
		suffix = "\n---"
	}
	if end < 0 {
		return nil, fmt.Errorf("SKILL.md frontmatter has no closing --- delimiter")
	}
	head := string(rest[:end])
	body := rest[end+len(suffix):]

	lines := strings.Split(head, "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "name:") {
			lines[i] = "name: " + newName
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append([]string{"name: " + newName}, lines...)
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.WriteString(strings.Join(lines, "\n"))
	out.WriteString(suffix)
	out.Write(body)
	return out.Bytes(), nil
}

// FrontmatterJSON serializes a parsed frontmatter map to compact JSON.
func FrontmatterJSON(fm map[string]any) (string, error) {
	b, err := json.Marshal(fm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
