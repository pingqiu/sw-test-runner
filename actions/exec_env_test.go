package actions

import "testing"

func TestPrefixEnvVars_NoEnvKeysReturnsCmdUnchanged(t *testing.T) {
	got := prefixEnvVars("bash run.sh", map[string]string{
		"cmd":    "bash run.sh",
		"target": "primary",
	})
	if got != "bash run.sh" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestPrefixEnvVars_BuildsSortedPrefix(t *testing.T) {
	got := prefixEnvVars("bash run.sh", map[string]string{
		"cmd":           "bash run.sh",
		"env.BAR":       "two",
		"env.FOO":       "one",
		"env.SW_ITER":   "5",
		"unrelated_key": "ignored",
	})
	want := "BAR=two FOO=one SW_ITER=5 bash run.sh"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestPrefixEnvVars_QuotesValuesWithSpacesAndSpecials(t *testing.T) {
	got := prefixEnvVars("run", map[string]string{
		"env.A": "a b",        // space → quote
		"env.B": "x$y",        // $ → quote
		"env.C": "plain",      // no special → no quote
		"env.D": `with'quote`, // single quote → escape
	})
	want := `A='a b' B='x$y' C=plain D='with'\''quote' run`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestPrefixEnvVars_EmptyValueIsEmptyQuotes(t *testing.T) {
	got := prefixEnvVars("run", map[string]string{
		"env.EMPTY": "",
	})
	if got != "EMPTY='' run" {
		t.Errorf("got %q, want \"EMPTY='' run\"", got)
	}
}

func TestPrefixEnvVars_RejectsBareEnvDot(t *testing.T) {
	// "env." with no name after the dot is malformed; should be ignored,
	// not produce a "= value" empty-name prefix.
	got := prefixEnvVars("run", map[string]string{
		"env.":    "lost",
		"env.OK":  "kept",
	})
	if got != "OK=kept run" {
		t.Errorf("got %q, want OK=kept run", got)
	}
}
