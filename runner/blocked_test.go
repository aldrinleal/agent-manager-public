package runner

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()

	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// The screen below was captured from a real session in manual mode that had
// just been asked to write a file.
func TestBlockedRecognisesARealPermissionPrompt(t *testing.T) {
	ok, why := ClaudeCode{}.Blocked(fixture(t, "permission-prompt.txt"))

	if !ok {
		t.Fatal("a real permission prompt was not recognised")
	}

	if why != "Do you want to create second-probe.txt?" {
		t.Errorf("reason = %q", why)
	}
}

// The same session a moment earlier, finished and idle, is not blocked.
func TestBlockedIgnoresAnIdleScreen(t *testing.T) {
	if ok, _ := (ClaudeCode{}).Blocked(fixture(t, "idle-screen.txt")); ok {
		t.Error("an idle prompt was reported as blocked")
	}
}

// The question varies by tool; the structure is what is matched.
func TestBlockedHandlesOtherQuestionsAndTheTrustPrompt(t *testing.T) {
	menu := func(q string) string {
		return "● Bash(rm -rf build)\n──────\n Bash command\n   rm -rf build\n\n " + q + "\n ❯ 1. Yes\n   2. Yes, and don't ask again for rm commands\n   3. No\n\n Esc to cancel · Tab to amend\n"
	}

	for _, q := range []string{"Do you want to proceed?", "Do you want to make this edit to main.go?", "Do you want to create new/file.txt?"} {
		if ok, why := (ClaudeCode{}).Blocked(menu(q)); !ok || why != q {
			t.Errorf("%q: ok=%v why=%q", q, ok, why)
		}
	}

	trust := " Do you trust the files in this folder?\n\n ❯ 1. Yes, proceed\n   2. No, exit\n\n Enter to confirm · Esc to cancel\n"
	if ok, _ := (ClaudeCode{}).Blocked(trust); !ok {
		t.Error("the folder-trust prompt blocks a launch and must be recognised")
	}
}

// Things that look similar but are not a live prompt.
func TestBlockedIsNotFooledByLookalikes(t *testing.T) {
	for name, screen := range map[string]string{
		// An answer that quotes a numbered list: no ❯ cursor, no footer.
		"prose list": "● Here are the options:\n  1. Yes, proceed with the migration\n  2. No, keep the current schema\nWhat do you prefer?\n❯ \n",
		// The input box showing text the user typed.
		"typed input": "❯ 1. Yes I think so\n⏸ manual mode on\n",
		"empty":       "",
		"blank lines": "\n\n\n   \n",
		// "Esc to cancel" on its own (another dialog), with no Yes/No menu.
		"footer only": "Select a model\n ❯ Sonnet\n   Opus\n Esc to cancel\n",
	} {
		if ok, _ := (ClaudeCode{}).Blocked(screen); ok {
			t.Errorf("%s: reported as blocked", name)
		}
	}
}

// An old, answered prompt scrolled up the screen must not count: only the
// bottom of the screen is considered.
func TestBlockedOnlyLooksAtTheBottomOfTheScreen(t *testing.T) {
	old := fixture(t, "permission-prompt.txt")
	filler := strings.Repeat("● more output from the agent\n", 40)

	if ok, _ := (ClaudeCode{}).Blocked(old + "\n" + filler + "❯ \n"); ok {
		t.Error("a prompt buried under 40 lines of later output is no longer live")
	}
}
