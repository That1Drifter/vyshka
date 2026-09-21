package main

import (
	"strings"
	"testing"
)

// A line the engine cut at 255 characters may have lost the digits of its
// last value, but only when the cut fell inside that value: a tail that is a
// partial field name proves the previous value ended at a tab.
func TestTruncatedLineSuspectsOnlyACutValue(t *testing.T) {
	pad := func(s string) string {
		return s + strings.Repeat("x", printLimit-len(s))
	}
	// Ends inside the value of writeTicks: the value is suspect.
	line := " SCRIPT       : VYSHKA_BPROBE\tstep=4\tevent=measured\tt=398804\tticks=892205753\tparseTicks=1\twriteTicks=1531"
	line = line[:len(line)-len("writeTicks=1531")] + "writeTicks=" + strings.Repeat("1", printLimit-len(line)+len("writeTicks=1531")-len("writeTicks="))
	if len(line) != printLimit {
		t.Fatalf("test line is %d long, want %d", len(line), printLimit)
	}
	ev, ok := parseEvent(line)
	if !ok || ev.suspect != "writeTicks" {
		t.Errorf("a value cut at the limit: suspect = %q, want writeTicks", ev.suspect)
	}

	// Ends inside the next field's name: writeTicks is complete.
	line = " SCRIPT       : VYSHKA_BPROBE\tstep=3\tevent=measured\tt=64054\tticks=1169038863\tparseTicks=573422895\twriteTicks=7219\twri"
	line = pad(line[:len(line)-3])[:printLimit-3] + "wri"
	if len(line) != printLimit {
		t.Fatalf("test line is %d long, want %d", len(line), printLimit)
	}
	ev, ok = parseEvent(line)
	if !ok || ev.suspect != "" {
		t.Errorf("a cut inside a field name: suspect = %q, want none", ev.suspect)
	}

	// A short line is never suspect.
	ev, ok = parseEvent("SCRIPT       : VYSHKA_BPROBE\tstep=0\tevent=measured\tt=224\tticks=234868375\twriteTicks=4161\twritten=true")
	if !ok || ev.suspect != "" {
		t.Errorf("a short line: suspect = %q, want none", ev.suspect)
	}
}

// The calibration must ignore any interval long enough for the 32-bit
// counter to have wrapped, or one wrapped step drags the median down and the
// ambiguity guard derived from it lets the same step through.
func TestCalibrationIgnoresIntervalsThatCouldWrap(t *testing.T) {
	mk := func(step int, event string, tms int, ticks int64) probeEvent {
		return probeEvent{step: step, event: event, t: tms, ticks: ticks, fields: map[string]string{}}
	}
	events := []probeEvent{
		mk(1, "fire", 0, 200000000),
		mk(1, "success", 100, 201000000),
		mk(1, "measured", 501100, 916032704),
		mk(1, "measured-more", 501101, 916042704),
	}
	got := ticksPerMs(events)
	if got < 9990 || got > 10010 {
		t.Errorf("ticks per ms = %.1f, want about 10000 from the 100 ms fetch alone", got)
	}
}

// A step is unfinished only when its completion records are missing: a boot
// that timed out in the gap after a complete measurement, or aborted on the
// next step's checkpoint, did not lose it.
func TestCompletedNeedsTheClosingRecords(t *testing.T) {
	fire := "SCRIPT       : VYSHKA_BPROBE\tstep=23\tevent=fire\tt=0\tticks=1\tlabel=bans-100-pad256k\tkind=bans\tpath=bans?n=100&pad=262144\tsize=100"
	success := "SCRIPT       : VYSHKA_BPROBE\tstep=23\tevent=success\tt=32\tticks=2\tsize=286117\tlen=286117"
	measured := "SCRIPT       : VYSHKA_BPROBE\tstep=23\tevent=measured\tt=16711\tticks=3\tphase=all\tok=1\tn=100\tparseTicks=165654328"
	more := "SCRIPT       : VYSHKA_BPROBE\tstep=23\tevent=measured-more\tt=16711\tticks=4\tserializeTicks=437904\twriteTicks=3532\twritten=true"
	abort := "SCRIPT       : VYSHKA_BPROBE\tstep=-1\tevent=abort\tt=0\tticks=5\treason=progress-unwritable\tat=24"

	if completed([]string{fire, success}, 23) {
		t.Error("a fetched but unmeasured ban step counts as completed")
	}
	if completed([]string{fire, success, measured}, 23) {
		t.Error("a ban step missing its second measurement line counts as completed")
	}
	if !completed([]string{fire, success, measured, more}, 23) {
		t.Error("a ban step with both measurement lines does not count as completed")
	}
	if !completed([]string{fire, success, measured, more, abort}, 23) {
		t.Error("the next step's checkpoint abort makes a completed step look unfinished")
	}
	failed := "SCRIPT       : VYSHKA_BPROBE\tstep=23\tevent=measured\tt=100\tticks=3\tphase=parse\tok=0\tparseTicks=5"
	if !completed([]string{fire, success, failed}, 23) {
		t.Error("a ban step whose parse failed does not count as completed")
	}
	rawFire := "SCRIPT       : VYSHKA_BPROBE\tstep=17\tevent=fire\tt=0\tticks=1\tlabel=raw-2m\tkind=raw\tpath=big?n=2097152\tsize=0"
	rawDone := "SCRIPT       : VYSHKA_BPROBE\tstep=17\tevent=success\tt=102\tticks=2\tsize=2097152\tlen=2097152"
	if completed([]string{rawFire}, 17) || !completed([]string{rawFire, rawDone}, 17) {
		t.Error("a raw step is completed by its fetch verdict and nothing less")
	}
}
