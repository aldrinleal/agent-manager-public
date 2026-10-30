package config

import (
	"bytes"
	"strings"
)

const (
	// descriptorKey is the ConfigMap key holding the inventory.
	descriptorKey = "    hosts.yaml: |"
	// descriptorIndent is how far the block's lines are indented in the
	// descriptor. Eight spaces: four for `data:` children, four more for the
	// literal block.
	descriptorIndent = 8
)

// RenderIntoDescriptor returns descriptor with the `hosts.yaml: |` literal
// block of its ConfigMap replaced by inventory.
//
// It edits TEXT, not a parsed document, on purpose. Round-tripping the
// descriptor through a YAML library would reorder keys and strip every
// comment in a file that is mostly comments explaining why things are the
// way they are. Only the lines between the key and the next sibling key
// change; everything else is preserved byte for byte.
//
// changed reports whether the output differs from the input, so callers can
// implement a --check mode without diffing themselves.
func RenderIntoDescriptor(descriptor, inventory []byte) (out []byte, changed bool, err error) {
	lines := strings.SplitAfter(string(descriptor), "\n")

	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\r\n") == descriptorKey {
			if start >= 0 {
				return nil, false, ErrInventory.New("descriptor has more than one %q block", strings.TrimSpace(descriptorKey))
			}
			start = i
		}
	}
	if start < 0 {
		return nil, false, ErrInventory.New("descriptor has no %q block", strings.TrimSpace(descriptorKey))
	}

	// The literal block runs until the first non-blank line indented less
	// than the block itself — i.e. the next sibling key (`known_hosts: |`)
	// or the end of the ConfigMap.
	end := start + 1
	for end < len(lines) {
		l := strings.TrimRight(lines[end], "\r\n")
		if strings.TrimSpace(l) != "" && leadingSpaces(l) < descriptorIndent {
			break
		}
		end++
	}

	// Blank lines between the block's last content line and the next key are
	// separators, not content: keep them where they were.
	contentEnd := end
	for contentEnd > start+1 && strings.TrimSpace(lines[contentEnd-1]) == "" {
		contentEnd--
	}

	var block strings.Builder

	body := strings.TrimRight(string(inventory), "\n")
	pad := strings.Repeat(" ", descriptorIndent)

	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == "" {
			block.WriteString("\n") // no trailing whitespace on blank lines
			continue
		}
		block.WriteString(pad + l + "\n")
	}

	var buf bytes.Buffer
	buf.WriteString(strings.Join(lines[:start+1], ""))
	buf.WriteString(block.String())
	buf.WriteString(strings.Join(lines[contentEnd:], ""))

	out = buf.Bytes()

	return out, !bytes.Equal(out, descriptor), nil
}

func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}

	return n
}
