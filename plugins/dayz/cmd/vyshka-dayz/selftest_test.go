package main

import (
	"strings"
	"testing"
)

// The self-test report is graded from the probe's lines alone, so what
// needs proving here is that every way the probe can fall short is named:
// a failed check, a probe that never finished (a fault takes the process
// before the finished line), and a plan the results do not add up to.

const scriptLogPrefix = "SCRIPT       : "

func feed(report *selfTestReport, lines ...string) bool {
	finished := false
	for _, line := range lines {
		finished = report.consume(scriptLogPrefix + line)
	}
	return finished
}

func TestSelfTestReportPassesWhenEveryPlannedCheckPassed(t *testing.T) {
	report := &selfTestReport{}
	finished := feed(report,
		"VYSHKA_SELFTEST\tplan=2",
		"[Vyshka] ERROR no usable config; the plugin is idle",
		"VYSHKA_SELFTEST\tcheck=files.bans\tresult=PASS\tentries=400\tsaveMs=12",
		"VYSHKA_SELFTEST\tcheck=json.speed\tresult=PASS\tbytes=1206414\tparseMs=900",
		"VYSHKA_SELFTEST\tfinished\tpassed=2\tfailed=0",
	)
	if !finished {
		t.Fatal("the finished line was not recognized")
	}
	if err := report.verdict(); err != nil {
		t.Fatalf("a clean run was graded as failed: %v", err)
	}
	if len(report.Results) != 2 || report.Results[1].Detail != "bytes=1206414 parseMs=900" {
		t.Fatalf("results not recorded as printed: %+v", report.Results)
	}
}

func TestSelfTestReportNamesAFailedCheck(t *testing.T) {
	report := &selfTestReport{}
	feed(report,
		"VYSHKA_SELFTEST\tplan=2",
		"VYSHKA_SELFTEST\tcheck=files.bans\tresult=PASS\tentries=400",
		"VYSHKA_SELFTEST\tcheck=json.longString\tresult=FAIL\tlength=8191\tequal=0",
		"VYSHKA_SELFTEST\tfinished\tpassed=1\tfailed=1",
	)
	err := report.verdict()
	if err == nil || !strings.Contains(err.Error(), "json.longString") {
		t.Fatalf("the failed check was not named: %v", err)
	}
}

func TestSelfTestReportFailsWhenTheProbeNeverFinished(t *testing.T) {
	report := &selfTestReport{}
	finished := feed(report,
		"VYSHKA_SELFTEST\tplan=3",
		"VYSHKA_SELFTEST\tcheck=files.bans\tresult=PASS\tentries=400",
	)
	if finished {
		t.Fatal("finished without the finished line")
	}
	err := report.verdict()
	if err == nil || !strings.Contains(err.Error(), "did not report finishing") || !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("the unfinished probe was not named: %v", err)
	}
}

func TestSelfTestReportFailsWhenThePlanDoesNotAddUp(t *testing.T) {
	report := &selfTestReport{}
	feed(report,
		"VYSHKA_SELFTEST\tplan=3",
		"VYSHKA_SELFTEST\tcheck=files.bans\tresult=PASS\tentries=400",
		"VYSHKA_SELFTEST\tfinished\tpassed=1\tfailed=0",
	)
	err := report.verdict()
	if err == nil || !strings.Contains(err.Error(), "planned 3") {
		t.Fatalf("the short plan was not named: %v", err)
	}
}

func TestInsertProbeCallsTheProbeFirstInMain(t *testing.T) {
	initC := "void main()\n{\n\tHive ce = CreateHive();\n\tLootDebug.Init();\n}\n"
	edited, err := insertProbe(initC, "class VyshkaSelfTest {}")
	if err != nil {
		t.Fatal(err)
	}
	want := "void main()\n{\n\tVyshkaSelfTest.Run();\n\tHive ce = CreateHive();\n}\n\n\nclass VyshkaSelfTest {}"
	if edited != want {
		t.Fatalf("init.c not edited as expected:\n%q\nwant\n%q", edited, want)
	}
	if _, err := insertProbe("void other()\n{\n}\n", "x"); err == nil {
		t.Fatal("a mission without main() was accepted")
	}
}
