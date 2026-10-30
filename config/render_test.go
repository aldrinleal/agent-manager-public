package config

import (
	"strings"
	"testing"
)

const sampleDescriptor = `apiVersion: v1
kind: ConfigMap
data:
    AGENTMGR_PORT: "8000"
    # a comment that must survive
    hosts.yaml: |
        old: stuff
          nested: true

        more: lines

    known_hosts: |
        host ssh-ed25519 AAAA
---
kind: StatefulSet
`

func TestRenderReplacesOnlyTheInventoryBlock(t *testing.T) {
	inv := "hosts:\n  - name: a\n\n  - name: b\n"

	out, changed, err := RenderIntoDescriptor([]byte(sampleDescriptor), []byte(inv))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected a change")
	}

	got := string(out)

	for _, keep := range []string{
		"    AGENTMGR_PORT: \"8000\"\n",
		"    # a comment that must survive\n",
		"    known_hosts: |\n        host ssh-ed25519 AAAA\n",
		"---\nkind: StatefulSet\n",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %q in:\n%s", keep, got)
		}
	}

	if strings.Contains(got, "old: stuff") {
		t.Errorf("old inventory survived:\n%s", got)
	}

	want := "    hosts.yaml: |\n        hosts:\n          - name: a\n\n          - name: b\n\n    known_hosts: |"
	if !strings.Contains(got, want) {
		t.Errorf("block wrong, want substring:\n%s\n--- got:\n%s", want, got)
	}
}

func TestRenderIsIdempotent(t *testing.T) {
	inv := []byte("hosts:\n  - name: a\n")

	once, _, err := RenderIntoDescriptor([]byte(sampleDescriptor), inv)
	if err != nil {
		t.Fatal(err)
	}

	twice, changed, err := RenderIntoDescriptor(once, inv)
	if err != nil {
		t.Fatal(err)
	}

	if changed || string(once) != string(twice) {
		t.Fatalf("second render changed the file:\n%s", twice)
	}
}

// Blank lines inside the inventory are written empty, never as eight spaces —
// trailing whitespace in a YAML literal block is preserved by the parser and
// would end up in the mounted file.
func TestRenderWritesNoTrailingWhitespace(t *testing.T) {
	out, _, err := RenderIntoDescriptor([]byte(sampleDescriptor), []byte("a: 1\n\nb: 2\n"))
	if err != nil {
		t.Fatal(err)
	}

	for i, l := range strings.Split(string(out), "\n") {
		if l != strings.TrimRight(l, " ") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, l)
		}
	}
}

func TestRenderRejectsMissingOrDuplicateBlock(t *testing.T) {
	if _, _, err := RenderIntoDescriptor([]byte("kind: ConfigMap\n"), []byte("x: 1\n")); err == nil {
		t.Error("expected an error with no hosts.yaml block")
	}

	dup := sampleDescriptor + "    hosts.yaml: |\n        x\n"
	if _, _, err := RenderIntoDescriptor([]byte(dup), []byte("x: 1\n")); err == nil {
		t.Error("expected an error with two blocks")
	}
}

// Block at the very end of the file, with no following key.
func TestRenderBlockAtEndOfFile(t *testing.T) {
	d := "data:\n    hosts.yaml: |\n        old: 1\n"

	out, _, err := RenderIntoDescriptor([]byte(d), []byte("new: 2\n"))
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "data:\n    hosts.yaml: |\n        new: 2\n" {
		t.Errorf("got %q", out)
	}
}
