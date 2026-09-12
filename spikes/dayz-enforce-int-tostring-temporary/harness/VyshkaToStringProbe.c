// Vyshka spike: what happens to int.ToString() inside a concatenation that
// also calls a method which itself calls ToString() (issue #50).
//
// Paste this into a mission init.c and call VyshkaToStringProbe.Run() from
// main(). Every case is a pure string expression over fixed inputs, so the
// expected value is known up front and the whole matrix runs synchronously at
// mission init. One machine readable line per case goes to the server script
// log:
//   VYSHKA_TSPROBE<TAB>case=<name><TAB>got=<text><TAB>want=<text><TAB>ok=<0|1>
//
// Everything runs server side; no game client, no stub server, no mod PBO.

class VyshkaToStringProbe
{
	// Same shape as VyshkaClock.Pad2 in the plugin: one branch concatenates
	// after ToString(), the other returns ToString() directly.
	static string Pad2(int value)
	{
		if (value < 10)
			return "0" + value.ToString();
		return value.ToString();
	}

	// Returns its argument untouched: a call with no ToString() inside.
	static string ProbeEcho(string text)
	{
		return text;
	}

	// A call whose only work is an int ToString().
	static string ProbeStringify(int value)
	{
		return value.ToString();
	}

	// A call that returns an int, so the caller concatenates string + int.
	static int ProbeTwice(int value)
	{
		return value * 2;
	}

	// A call whose only work is a float ToString().
	static string ProbeFloatStr(float value)
	{
		return value.ToString();
	}

	static string ProbeConcat(string a, string b)
	{
		return a + b;
	}

	// The plugin's NowCompact expression, verbatim, over caller supplied ints.
	static string CaseDirectChain(int year, int month, int day)
	{
		return year.ToString() + Pad2(month) + Pad2(day);
	}

	// The plugin's NowRfc3339 shape, which was observed to work.
	static string CaseLiteralBetween(int year, int month, int day)
	{
		return year.ToString() + "-" + Pad2(month) + "-" + Pad2(day);
	}

	static string CaseTwoToString(int year, int month)
	{
		return year.ToString() + month.ToString();
	}

	static string CaseThreeToString(int year, int month, int day)
	{
		return year.ToString() + month.ToString() + day.ToString();
	}

	static string CaseToStringThenPlainCall(int year)
	{
		return year.ToString() + ProbeEcho("x");
	}

	static string CaseToStringThenStringify(int year, int day)
	{
		return year.ToString() + ProbeStringify(day);
	}

	static string CaseToStringThenPadLow(int year, int month)
	{
		return year.ToString() + Pad2(month);
	}

	static string CaseToStringThenPadHigh(int year, int day)
	{
		return year.ToString() + Pad2(day);
	}

	static string CaseToStringThenIntCall(int year, int day)
	{
		return year.ToString() + ProbeTwice(day);
	}

	static string CaseToStringThenFloatCall(int year)
	{
		return year.ToString() + ProbeFloatStr(1.5);
	}

	static string CaseFloatToStringThenPad(int month)
	{
		float f = 1.5;
		return f.ToString() + Pad2(month);
	}

	static string CaseCallThenToString(int year, int month)
	{
		return Pad2(month) + year.ToString();
	}

	static string CaseToStringAsArgument(int year, int month)
	{
		return ProbeConcat(year.ToString(), Pad2(month));
	}

	static string CaseParenthesized(int year, int month, int day)
	{
		return year.ToString() + (Pad2(month) + Pad2(day));
	}

	// Candidate fix 1: copy into a local before the calls.
	static string CaseLocalCopy(int year, int month, int day)
	{
		string y = year.ToString();
		return y + Pad2(month) + Pad2(day);
	}

	// Candidate fix 2: build with append assignments.
	static string CaseAppendAssign(int year, int month, int day)
	{
		string s = year.ToString();
		s += Pad2(month);
		s += Pad2(day);
		return s;
	}

	// Candidate fix 3: let the concatenation operator convert the int.
	static string CaseImplicitIntConcat(int year, int month, int day)
	{
		return "" + year + Pad2(month) + Pad2(day);
	}

	// Candidate fix 4: string.Format.
	static string CaseFormat(int year, int month, int day)
	{
		return string.Format("%1%2%3", year, Pad2(month), Pad2(day));
	}

	static void Check(string name, string got, string want)
	{
		int ok = 0;
		if (got == want)
			ok = 1;
		Print("VYSHKA_TSPROBE\tcase=" + name + "\tgot=" + got + "\twant=" + want + "\tok=" + ok);
	}

	static void Run()
	{
		int year = 2026;
		int month = 9;
		int day = 11;
		Print("VYSHKA_TSPROBE\tcase=boot\tgot=start\twant=start\tok=1");

		// The defect and its known-good sibling.
		Check("direct-chain", CaseDirectChain(year, month, day), "20260911");
		Check("literal-between", CaseLiteralBetween(year, month, day), "2026-09-11");

		// What exactly clobbers the left operand.
		Check("two-tostring", CaseTwoToString(year, month), "20269");
		Check("three-tostring", CaseThreeToString(year, month, day), "2026911");
		Check("tostring-then-plain-call", CaseToStringThenPlainCall(year), "2026x");
		Check("tostring-then-stringify", CaseToStringThenStringify(year, day), "202611");
		Check("tostring-then-pad-low", CaseToStringThenPadLow(year, month), "202609");
		Check("tostring-then-pad-high", CaseToStringThenPadHigh(year, day), "202611");
		Check("tostring-then-int-call", CaseToStringThenIntCall(year, day), "202622");
		Check("tostring-then-float-call", CaseToStringThenFloatCall(year), "20261.5");
		Check("float-tostring-then-pad", CaseFloatToStringThenPad(month), "1.509");
		Check("call-then-tostring", CaseCallThenToString(year, month), "092026");
		Check("tostring-as-argument", CaseToStringAsArgument(year, month), "202609");
		Check("parenthesized", CaseParenthesized(year, month, day), "20260911");

		// Candidate fixes.
		Check("local-copy", CaseLocalCopy(year, month, day), "20260911");
		Check("append-assign", CaseAppendAssign(year, month, day), "20260911");
		Check("implicit-int-concat", CaseImplicitIntConcat(year, month, day), "20260911");
		Check("format", CaseFormat(year, month, day), "20260911");

		// The live clock, the way the plugin reads it, so the defect is seen
		// against real out-parameter ints and not only literals.
		int ly, lm, ld;
		GetYearMonthDayUTC(ly, lm, ld);
		string wy = ly.ToString();
		string wm = Pad2(lm);
		string wd = Pad2(ld);
		Print("VYSHKA_TSPROBE\tcase=live-clock-ints\tgot=" + ly + "/" + lm + "/" + ld + "\twant=" + ly + "/" + lm + "/" + ld + "\tok=1");
		Check("live-clock-direct-chain", CaseDirectChain(ly, lm, ld), wy + wm + wd);
		Check("live-clock-local-copy", CaseLocalCopy(ly, lm, ld), wy + wm + wd);

		Print("VYSHKA_TSPROBE\tcase=done\tgot=end\twant=end\tok=1");
	}
}
