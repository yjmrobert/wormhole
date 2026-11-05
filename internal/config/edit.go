package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// The commands that write config.yaml (join, init, senders add and remove)
// edit it as a YAML document tree, so the comments that document every
// key survive the edit and a hand-written file keeps its shape.

// Edit loads path as a YAML document (or starts from Example when it does
// not exist and create is set), applies fn and writes the result back with
// mode 600, atomically.
func Edit(path string, create bool, fn func(doc *yaml.Node) error) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		raw = exampleFor(path)
	} else if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return editRaw(path, raw, fn)
}

// Create writes path from Example with fn applied, whatever is there now:
// `wormhole init --force` means a fresh start, not an edit.
func Create(path string, fn func(doc *yaml.Node) error) error {
	return editRaw(path, exampleFor(path), fn)
}

// exampleFor is Example with its credential paths pointed at path's
// directory.
func exampleFor(path string) []byte {
	return []byte(strings.ReplaceAll(Example, "~/.config/wormhole", filepath.Dir(path)))
}

// editRaw applies fn to a YAML document and writes it to path.
func editRaw(path string, raw []byte, fn func(doc *yaml.Node) error) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if err := fn(&doc); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	// Validate before writing: a config this binary cannot load is never
	// left on disk.
	if _, err := Parse(buf.Bytes()); err != nil {
		return err
	}
	return WritePrivate(path, buf.Bytes())
}

// WritePrivate writes content to path with mode 600, atomically, creating
// the directory with mode 700.
func WritePrivate(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// root returns the document's top-level mapping.
func root(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

// lookup finds key in a mapping node and returns its value node.
func lookup(m *yaml.Node, key string) (*yaml.Node, bool) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], true
		}
	}
	return nil, false
}

// ensure finds or appends key in a mapping and returns its value node,
// creating it with the given kind.
func ensure(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	if v, ok := lookup(m, key); ok {
		if v.Kind != kind || v.Tag == "!!null" {
			v.Kind, v.Tag, v.Value, v.Content, v.Style = kind, "", "", nil, 0
			if kind == yaml.MappingNode {
				v.Tag = "!!map"
			} else if kind == yaml.SequenceNode {
				v.Tag = "!!seq"
			}
		}
		return v
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: kind}
	switch kind {
	case yaml.MappingNode:
		v.Tag = "!!map"
	case yaml.SequenceNode:
		v.Tag = "!!seq"
	}
	m.Content = append(m.Content, k, v)
	return v
}

// Set writes a scalar at a dotted path (bus.url), creating mappings on
// the way. The scalar keeps its line comment when it already exists.
func Set(doc *yaml.Node, dotted, value string) {
	m := root(doc)
	parts := strings.Split(dotted, ".")
	for _, p := range parts[:len(parts)-1] {
		m = ensure(m, p, yaml.MappingNode)
	}
	v := ensure(m, parts[len(parts)-1], yaml.ScalarNode)
	v.Tag = ""
	v.Style = 0
	v.Value = value
	if value == "" || strings.ContainsAny(value, ":#{}[],&*!|>'\"%@`") || strings.TrimSpace(value) != value {
		v.Style = yaml.DoubleQuotedStyle
	}
}

// SetList writes a sequence of scalars at a dotted path.
func SetList(doc *yaml.Node, dotted string, values []string) {
	m := root(doc)
	parts := strings.Split(dotted, ".")
	for _, p := range parts[:len(parts)-1] {
		m = ensure(m, p, yaml.MappingNode)
	}
	v := ensure(m, parts[len(parts)-1], yaml.SequenceNode)
	v.Content = nil
	v.Style = 0
	for _, s := range values {
		v.Content = append(v.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s})
	}
	if len(values) == 0 {
		v.Style = yaml.FlowStyle
	}
}

// Unset removes the key at a dotted path, if present.
func Unset(doc *yaml.Node, dotted string) {
	m := root(doc)
	parts := strings.Split(dotted, ".")
	for _, p := range parts[:len(parts)-1] {
		v, ok := lookup(m, p)
		if !ok {
			return
		}
		m = v
	}
	key := parts[len(parts)-1]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// Has reports whether a key exists at a dotted path.
func Has(doc *yaml.Node, dotted string) bool {
	m := root(doc)
	parts := strings.Split(dotted, ".")
	for _, p := range parts[:len(parts)-1] {
		v, ok := lookup(m, p)
		if !ok {
			return false
		}
		m = v
	}
	_, ok := lookup(m, parts[len(parts)-1])
	return ok
}

// Get reads a scalar at a dotted path.
func Get(doc *yaml.Node, dotted string) (string, bool) {
	m := root(doc)
	parts := strings.Split(dotted, ".")
	for _, p := range parts[:len(parts)-1] {
		v, ok := lookup(m, p)
		if !ok {
			return "", false
		}
		m = v
	}
	v, ok := lookup(m, parts[len(parts)-1])
	if !ok || v.Kind != yaml.ScalarNode {
		return "", false
	}
	return v.Value, true
}

// senderHandle reads the handle of one senders entry.
func senderHandle(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	if h, ok := lookup(n, "handle"); ok {
		return h.Value
	}
	return ""
}

// AddSenderNode appends handle under senders in inbox mode (a bare
// scalar) or auto mode (a flow mapping). It reports false when the handle
// was already listed, in which case nothing changes.
func AddSenderNode(doc *yaml.Node, handle, mode string) bool {
	seq := ensure(root(doc), "senders", yaml.SequenceNode)
	for _, n := range seq.Content {
		if senderHandle(n) == handle {
			return false
		}
	}
	seq.Style = 0
	var entry *yaml.Node
	if mode == ModeAuto {
		entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "handle"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: handle},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mode"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: ModeAuto},
		}}
	} else {
		entry = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: handle}
	}
	seq.Content = append(seq.Content, entry)
	return true
}

// RemoveSenderNode deletes handle from senders and reports whether it was
// there.
func RemoveSenderNode(doc *yaml.Node, handle string) bool {
	seq, ok := lookup(root(doc), "senders")
	if !ok || seq.Kind != yaml.SequenceNode {
		return false
	}
	kept := seq.Content[:0]
	found := false
	for _, n := range seq.Content {
		if senderHandle(n) == handle {
			found = true
			continue
		}
		kept = append(kept, n)
	}
	seq.Content = kept
	if len(kept) == 0 {
		seq.Style = yaml.FlowStyle
	}
	return found
}

// AddSender adds handle to the senders list in the file at path.
func AddSender(path, handle, mode string) error {
	return Edit(path, false, func(doc *yaml.Node) error {
		if !AddSenderNode(doc, handle, mode) {
			return fmt.Errorf("%w: ^%s is already under senders: in %s", ErrExists, handle, path)
		}
		return nil
	})
}

// RemoveSender removes handle from the senders list in the file at path.
func RemoveSender(path, handle string) error {
	return Edit(path, false, func(doc *yaml.Node) error {
		if !RemoveSenderNode(doc, handle) {
			return fmt.Errorf("%w: ^%s is not under senders: in %s", ErrNotListed, handle, path)
		}
		return nil
	})
}

// Errors from AddSender and RemoveSender.
var (
	ErrExists    = errors.New("already listed")
	ErrNotListed = errors.New("not listed")
)
