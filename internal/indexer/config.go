package indexer

import (
	"bufio"
	"bytes"
	"path"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/yaml"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Configuration files: Spring's application and bootstrap files (YAML or
// properties, with profile variants such as application-prod.yml) and
// .env files. Their keys and values let the store resolve the names
// services are configured with (a Kafka topic "${orders.topic}", a base
// URL from os.Getenv("INVENTORY_URL")) into the names other services use.
//
// Keys that look like secrets are never stored: the values are only used
// to name topics, queues and URLs.

var (
	springConfig = regexp.MustCompile(`^(application|bootstrap)(-[\w.-]+)?\.(ya?ml|properties)$`)
	dotEnv       = regexp.MustCompile(`^\.env(\.[\w.-]+)?$`)
	secretKey    = regexp.MustCompile(`(?i)(secret|passw|token|credential|private|api[_.-]?key|access[_.-]?key|auth|cert)`)
)

// isConfigFile reports whether a file name is a configuration file codeweft
// reads values from.
func isConfigFile(name string) bool {
	return springConfig.MatchString(name) || dotEnv.MatchString(name)
}

// ParseConfig reads the properties of a configuration file.
func ParseConfig(rel string, src []byte) (*FileResult, error) {
	base := path.Base(rel)
	res := &FileResult{File: store.File{Path: rel, Dir: path.Dir(rel), Lang: "config", Qual: rel}}
	profile := ""
	if m := springConfig.FindStringSubmatch(base); m != nil {
		profile = strings.TrimPrefix(m[2], "-")
	}
	add := func(key, value string, line int, profile string) {
		key = strings.TrimSpace(key)
		if key == "" || secretKey.MatchString(key) {
			return
		}
		res.Properties = append(res.Properties, store.Property{Key: key, Value: strings.TrimSpace(value), Line: line, Profile: profile})
	}
	switch {
	case strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml"):
		if err := parseYAMLConfig(src, profile, add); err != nil {
			return nil, err
		}
	case strings.HasSuffix(base, ".properties"):
		parseProperties(src, func(k, v string, line int) { add(k, v, line, profile) })
	default: // .env
		profile = strings.TrimPrefix(strings.TrimPrefix(base, ".env"), ".")
		parseDotEnv(src, func(k, v string, line int) { add(k, v, line, profile) })
	}
	return res, nil
}

// parseYAMLConfig flattens YAML mappings into dotted keys (app.orders.topic).
// A sequence of scalars becomes their comma-separated values, as Spring
// binds them. A document activated by a profile (spring.config.activate.on-profile,
// or the older spring.profiles) gives its keys that profile.
func parseYAMLConfig(src []byte, fileProfile string, add func(key, value string, line int, profile string)) error {
	root, done, err := parseTree(yaml.GetLanguage(), src)
	if err != nil {
		return err
	}
	defer done()
	for _, doc := range namedChildren(root) {
		if doc.Type() != "document" {
			continue
		}
		type kv struct {
			key, value string
			line       int
		}
		var pairs []kv
		var walk func(n *sitter.Node, prefix string)
		walk = func(n *sitter.Node, prefix string) {
			if n == nil {
				return
			}
			switch n.Type() {
			case "document", "block_node", "flow_node":
				for _, c := range namedChildren(n) {
					walk(c, prefix)
				}
			case "block_mapping", "flow_mapping":
				for _, pair := range namedChildren(n) {
					if pair.Type() != "block_mapping_pair" && pair.Type() != "flow_pair" {
						continue
					}
					key := yamlScalar(field(pair, "key"), src)
					if key == "" {
						continue
					}
					if prefix != "" {
						key = prefix + "." + key
					}
					value := field(pair, "value")
					if s, ok := yamlValue(value, src); ok {
						pairs = append(pairs, kv{key, s, line(pair)})
					} else {
						walk(value, key)
					}
				}
			}
		}
		walk(doc, "")
		profile := fileProfile
		for _, p := range pairs {
			if p.key == "spring.config.activate.on-profile" || p.key == "spring.profiles" {
				profile = p.value
			}
		}
		for _, p := range pairs {
			add(p.key, p.value, p.line, profile)
		}
	}
	return nil
}

// yamlValue returns a scalar value, or the comma-separated values of a
// sequence of scalars; ok is false for a mapping.
func yamlValue(n *sitter.Node, src []byte) (string, bool) {
	if n == nil {
		return "", true
	}
	for (n.Type() == "flow_node" || n.Type() == "block_node") && n.NamedChildCount() == 1 {
		n = n.NamedChild(0)
	}
	switch n.Type() {
	case "block_mapping", "flow_mapping":
		return "", false
	case "block_sequence", "flow_sequence":
		var items []string
		for _, c := range namedChildren(n) {
			if c.Type() == "block_sequence_item" && c.NamedChildCount() > 0 {
				c = c.NamedChild(0)
			}
			if s, ok := yamlValue(c, src); ok {
				items = append(items, s)
			}
		}
		return strings.Join(items, ","), true
	}
	return yamlScalar(n, src), true
}

// yamlScalar returns the text of a scalar, unquoted.
func yamlScalar(n *sitter.Node, src []byte) string {
	for n != nil && (n.Type() == "flow_node" || n.Type() == "block_node") && n.NamedChildCount() > 0 {
		n = n.NamedChild(0)
	}
	if n == nil {
		return ""
	}
	t := text(n, src)
	switch n.Type() {
	case "double_quote_scalar":
		t = strings.TrimSuffix(strings.TrimPrefix(t, `"`), `"`)
		t = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t").Replace(t)
	case "single_quote_scalar":
		t = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(t, `'`), `'`), `''`, `'`)
	case "block_scalar":
		lines := strings.Split(t, "\n")
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		t = strings.TrimSpace(strings.Join(lines[1:], "\n")) // drop the | or > indicator line
	}
	return t
}

// parseProperties reads a Java properties file: key=value or key: value,
// # and ! comments, lines continued with a trailing backslash.
func parseProperties(src []byte, add func(key, value string, line int)) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	n, start := 0, 0
	var logical strings.Builder
	for sc.Scan() {
		n++
		l := strings.TrimLeft(sc.Text(), " \t")
		if logical.Len() == 0 {
			if l == "" || l[0] == '#' || l[0] == '!' {
				continue
			}
			start = n
		}
		if strings.HasSuffix(l, `\`) && !strings.HasSuffix(l, `\\`) {
			logical.WriteString(strings.TrimSuffix(l, `\`))
			continue
		}
		logical.WriteString(l)
		entry := logical.String()
		logical.Reset()
		i := strings.IndexAny(entry, "=:")
		if i < 0 {
			continue
		}
		add(entry[:i], entry[i+1:], start)
	}
}

// parseDotEnv reads a .env file: KEY=VALUE lines, optionally prefixed with
// export, values optionally quoted, # comments.
func parseDotEnv(src []byte, add func(key, value string, line int)) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" || l[0] == '#' {
			continue
		}
		l = strings.TrimPrefix(l, "export ")
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0]:
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		add(k, v, n)
	}
}
