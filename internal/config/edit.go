package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yjmrobert/wormhole/internal/envelope"
)

// ErrNoSender is returned by RemoveSender when the handle is not listed.
var ErrNoSender = errors.New("not under senders:")

// AddSender puts handle under senders: in the config file at path with
// mode (inbox or auto). A handle already listed keeps its place and takes
// the new mode. Comments, blank lines and key order survive, and the result
// must validate before it replaces the file.
func AddSender(path, handle, mode string) error {
	if !envelope.ValidHandle(handle) {
		return fmt.Errorf("%w: sender %q is not a valid handle", ErrInvalid, handle)
	}
	if mode != ModeInbox && mode != ModeAuto {
		return fmt.Errorf("%w: sender mode %q (want inbox or auto)", ErrInvalid, mode)
	}
	return editSenders(path, func(e *sendersEdit) error {
		if i := findSender(e.seq, handle); i >= 0 {
			return e.replace(i, handle, mode)
		}
		return e.add(handle, mode)
	})
}

// RemoveSender takes handle out of senders: in the config file at path. It
// returns ErrNoSender when the handle is not listed.
func RemoveSender(path, handle string) error {
	return editSenders(path, func(e *sendersEdit) error {
		i := findSender(e.seq, handle)
		if i < 0 {
			return fmt.Errorf("%s %w", handle, ErrNoSender)
		}
		return e.remove(i)
	})
}

// sendersEdit is one change to the senders: list. The common shapes (a
// block list of one-line entries, a one-line flow list, an empty or
// missing key) are edited as text at the positions the parser reports, so
// the rest of the file stays byte for byte. Anything else is re-encoded
// from the node tree, which keeps comments and order but not blank lines.
type sendersEdit struct {
	doc   *yaml.Node
	root  *yaml.Node
	key   *yaml.Node // nil when the file has no senders: key
	seq   *yaml.Node
	lines []string // the file, each line with its newline
	// text is false once an edit fell back to the node tree.
	text bool
}

// entryText is how one sender is written.
func entryText(handle, mode string) string {
	if mode == ModeAuto {
		return fmt.Sprintf("{handle: %s, mode: %s}", handle, mode)
	}
	return handle
}

// textual reports whether the list can be edited as text: a block list of
// one-line entries or a flow list on the key's line.
func (e *sendersEdit) textual() bool {
	if e.key == nil || e.seq.Kind != yaml.SequenceNode {
		return true
	}
	for _, n := range e.seq.Content {
		if endLine(n) != n.Line || n.Line < 1 || n.Line > len(e.lines) {
			return false
		}
	}
	if e.seq.Style&yaml.FlowStyle != 0 {
		return e.seq.Line == e.key.Line && strings.Count(e.lines[e.key.Line-1], "[") == 1 && strings.Count(e.lines[e.key.Line-1], "]") == 1
	}
	for _, n := range e.seq.Content {
		if !strings.HasPrefix(strings.TrimSpace(e.lines[n.Line-1]), "- ") {
			return false
		}
	}
	return len(e.seq.Content) > 0
}

// add appends a sender.
func (e *sendersEdit) add(handle, mode string) error {
	entry := entryText(handle, mode)
	if !e.text {
		e.nodeSeq().Content = append(e.nodeSeq().Content, senderNode(handle, mode))
		if len(e.seq.Content) == 1 {
			e.seq.Style = 0
		}
		return nil
	}
	if e.key == nil {
		if n := len(e.lines); n > 0 && !strings.HasSuffix(e.lines[n-1], "\n") {
			e.lines[n-1] += "\n"
		}
		e.lines = append(e.lines, "senders:\n", "  - "+entry+"\n")
		return nil
	}
	if e.seq.Kind != yaml.SequenceNode || e.seq.Style&yaml.FlowStyle != 0 {
		// senders:, senders: ~ or senders: [a, b] become a block list.
		items := []string{}
		if e.seq.Kind == yaml.SequenceNode {
			for _, n := range e.seq.Content {
				items = append(items, flowText(n))
			}
		}
		items = append(items, entry)
		e.setKeyLine("", items)
		return nil
	}
	last := e.seq.Content[len(e.seq.Content)-1]
	e.insertAfter(last.Line, indentOf(e.lines[last.Line-1])+"- "+entry+"\n")
	return nil
}

// replace rewrites entry i with a new mode.
func (e *sendersEdit) replace(i int, handle, mode string) error {
	if !e.text {
		old := e.seq.Content[i]
		n := senderNode(handle, mode)
		n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
		e.seq.Content[i] = n
		return nil
	}
	n := e.seq.Content[i]
	if e.seq.Style&yaml.FlowStyle != 0 {
		items := []string{}
		for j, c := range e.seq.Content {
			if j == i {
				items = append(items, entryText(handle, mode))
			} else {
				items = append(items, flowText(c))
			}
		}
		e.setKeyLine("", items)
		return nil
	}
	line := e.lines[n.Line-1]
	comment := ""
	if n.LineComment != "" {
		comment = " " + n.LineComment
	}
	e.lines[n.Line-1] = indentOf(line) + "- " + entryText(handle, mode) + comment + "\n"
	return nil
}

// remove deletes entry i.
func (e *sendersEdit) remove(i int) error {
	if !e.text {
		e.seq.Content = append(e.seq.Content[:i], e.seq.Content[i+1:]...)
		if len(e.seq.Content) == 0 {
			e.seq.Style = yaml.FlowStyle
		}
		return nil
	}
	if e.seq.Style&yaml.FlowStyle != 0 || len(e.seq.Content) == 1 {
		items := []string{}
		for j, c := range e.seq.Content {
			if j != i {
				items = append(items, flowText(c))
			}
		}
		if e.seq.Style&yaml.FlowStyle == 0 {
			e.deleteLine(e.seq.Content[i].Line)
		}
		e.setKeyLine("[]", items)
		return nil
	}
	e.deleteLine(e.seq.Content[i].Line)
	return nil
}

// setKeyLine rewrites the senders: line, keeping any comment on it, and
// puts items under it as a block list, or writes empty when there are none.
func (e *sendersEdit) setKeyLine(empty string, items []string) {
	idx := e.key.Line - 1
	line := strings.TrimRight(e.lines[idx], "\r\n")
	col := e.key.Column - 1 + len(e.key.Value)
	if e.key.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		col += 2
	}
	colon := col + strings.Index(line[col:], ":")
	head := line[:colon+1]
	comment := ""
	if h := strings.Index(line[colon+1:], "#"); h >= 0 {
		comment = " " + line[colon+1+h:]
	}
	if len(items) == 0 {
		e.lines[idx] = head + " " + empty + comment + "\n"
		return
	}
	block := []string{head + comment + "\n"}
	indent := strings.Repeat(" ", e.key.Column-1) + "  "
	for _, it := range items {
		block = append(block, indent+"- "+it+"\n")
	}
	e.lines = append(e.lines[:idx], append(block, e.lines[idx+1:]...)...)
}

// insertAfter inserts text after the 1-based line n.
func (e *sendersEdit) insertAfter(n int, text string) {
	e.lines = append(e.lines[:n], append([]string{text}, e.lines[n:]...)...)
}

// deleteLine removes the 1-based line n.
func (e *sendersEdit) deleteLine(n int) {
	e.lines = append(e.lines[:n-1], e.lines[n:]...)
}

// nodeSeq is the senders: sequence node, created for the node fallback.
func (e *sendersEdit) nodeSeq() *yaml.Node {
	e.seq = sendersNode(e.root)
	return e.seq
}

// bytes renders the edited file.
func (e *sendersEdit) bytes() ([]byte, error) {
	if e.text {
		return []byte(strings.Join(e.lines, "")), nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(e.doc); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return buf.Bytes(), nil
}

// editSenders loads path, applies fn to its senders: list, validates the
// result and writes it back atomically with mode 0600.
func editSenders(path string, fn func(e *sendersEdit) error) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if doc.Kind != yaml.DocumentNode || root.Kind != yaml.MappingNode {
		return fmt.Errorf("%w: the top level of %s is not a mapping", ErrInvalid, path)
	}
	e := &sendersEdit{doc: &doc, root: root, seq: &yaml.Node{Kind: yaml.SequenceNode}, lines: strings.SplitAfter(string(raw), "\n")}
	if n := len(e.lines); n > 0 && e.lines[n-1] == "" {
		e.lines = e.lines[:n-1]
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "senders" {
			e.key, e.seq = root.Content[i], root.Content[i+1]
		}
	}
	e.text = e.textual()
	if !e.text {
		e.nodeSeq()
	}
	if err := fn(e); err != nil {
		return err
	}
	out, err := e.bytes()
	if err != nil {
		return err
	}
	if _, err := Parse(out); err != nil {
		return err
	}
	return writeAtomic(path, out)
}

// endLine is the last line any part of n is on.
func endLine(n *yaml.Node) int {
	last := n.Line
	for _, c := range n.Content {
		last = max(last, endLine(c))
	}
	return last
}

// indentOf is the leading whitespace of line.
func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// flowText renders a sender node on one line.
func flowText(n *yaml.Node) string {
	c := *n
	c.HeadComment, c.LineComment, c.FootComment = "", "", ""
	if c.Kind == yaml.MappingNode {
		c.Style = yaml.FlowStyle
	}
	b, err := yaml.Marshal(&c)
	if err != nil {
		return n.Value
	}
	return strings.TrimSpace(string(b))
}

// sendersNode returns the senders: sequence of root, turning a missing or
// null value into an empty one.
func sendersNode(root *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "senders" {
			continue
		}
		v := root.Content[i+1]
		if v.Kind != yaml.SequenceNode {
			seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", LineComment: v.LineComment, FootComment: v.FootComment}
			root.Content[i+1] = seq
			return seq
		}
		return v
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "senders", HeadComment: "# Who may open a wormhole to you; edited by: wormhole senders add|remove"}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	root.Content = append(root.Content, key, seq)
	return seq
}

// findSender is the index of handle in seq, or -1.
func findSender(seq *yaml.Node, handle string) int {
	for i, n := range seq.Content {
		switch n.Kind {
		case yaml.ScalarNode:
			if n.Value == handle {
				return i
			}
		case yaml.MappingNode:
			for j := 0; j+1 < len(n.Content); j += 2 {
				if n.Content[j].Value == "handle" && n.Content[j+1].Value == handle {
					return i
				}
			}
		}
	}
	return -1
}

// senderNode is a bare handle for inbox mode and {handle, mode} otherwise.
func senderNode(handle, mode string) *yaml.Node {
	if mode == ModeInbox {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: handle}
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "handle"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: handle},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mode"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: mode},
	}}
}

// writeAtomic replaces path with data, mode 0600, through a temporary file
// in the same directory.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}
