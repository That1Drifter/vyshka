// Vyshka DayZ plugin: wall clock, RFC 3339 timestamps, and envelope ids.
//
// The engine exposes the UTC system date and time as separate integers, which
// is enough to emit the RFC 3339 UTC timestamp every envelope needs (spec
// section 4) and to compare an action's expiresAt against now (section 7).
// Epoch seconds are held in a 32-bit int, which the engine's script ints are;
// that is fine until 2038 and the plugin does not pretend otherwise.

class VyshkaClock
{
	// Seconds since the game process started, from the engine's millisecond
	// tick counter; for measuring durations and backoff, not for timestamps.
	static int MonotonicMs()
	{
		return GetGame().GetTime();
	}

	static string Pad2(int value)
	{
		if (value < 10)
			return "0" + value.ToString();
		return value.ToString();
	}

	// Both formatters copy the year into a local before calling Pad2. An
	// int.ToString() still pending in an expression takes the value of the
	// next int.ToString() run inside a function the expression calls, so
	// year.ToString() + Pad2(month) yields the month twice and no year
	// (spikes/dayz-enforce-int-tostring-temporary). A literal concatenated
	// first also avoids it, which is why the RFC 3339 shape happened to work.

	// NowRfc3339 formats the current UTC time as 2026-09-03T16:50:19Z.
	static string NowRfc3339()
	{
		int year, month, day, hour, minute, second;
		GetYearMonthDayUTC(year, month, day);
		GetHourMinuteSecondUTC(hour, minute, second);
		string y = year.ToString();
		return y + "-" + Pad2(month) + "-" + Pad2(day) + "T" + Pad2(hour) + ":" + Pad2(minute) + ":" + Pad2(second) + "Z";
	}

	// NowCompact formats the current UTC time as 20260903T165019, for ids.
	static string NowCompact()
	{
		int year, month, day, hour, minute, second;
		GetYearMonthDayUTC(year, month, day);
		GetHourMinuteSecondUTC(hour, minute, second);
		string y = year.ToString();
		return y + Pad2(month) + Pad2(day) + "T" + Pad2(hour) + Pad2(minute) + Pad2(second);
	}

	// EpochSeconds is the current UTC time as seconds since 1970-01-01.
	static int EpochSeconds()
	{
		int year, month, day, hour, minute, second;
		GetYearMonthDayUTC(year, month, day);
		GetHourMinuteSecondUTC(hour, minute, second);
		return DaysFromCivil(year, month, day) * 86400 + hour * 3600 + minute * 60 + second;
	}

	// DaysFromCivil counts days from 1970-01-01 to the given proleptic
	// Gregorian date (negative before it).
	static int DaysFromCivil(int year, int month, int day)
	{
		if (month <= 2)
			year -= 1;
		int era;
		if (year >= 0)
			era = year / 400;
		else
			era = (year - 399) / 400;
		int yoe = year - era * 400;
		int mp = (month + 9) % 12;
		int doy = (153 * mp + 2) / 5 + day - 1;
		int doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
		return era * 146097 + doe - 719468;
	}

	// ParseRfc3339 reads a timestamp like 2026-09-03T16:50:19.123Z or
	// 2026-09-03T16:50:19+02:00 into epoch seconds. Returns false on anything
	// it cannot read; callers treat that as "no deadline known".
	static bool ParseRfc3339(string text, out int epoch)
	{
		if (text.Length() < 20)
			return false;
		int year = ReadInt(text, 0, 4);
		int month = ReadInt(text, 5, 2);
		int day = ReadInt(text, 8, 2);
		int hour = ReadInt(text, 11, 2);
		int minute = ReadInt(text, 14, 2);
		int second = ReadInt(text, 17, 2);
		if (year < 0 || month < 1 || month > 12 || day < 1 || day > 31 || hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 60)
			return false;
		if (text.Get(4) != "-" || text.Get(7) != "-" || text.Get(13) != ":" || text.Get(16) != ":")
			return false;
		string t = text.Get(10);
		if (t != "T" && t != "t" && t != " ")
			return false;

		int pos = 19;
		if (pos < text.Length() && text.Get(pos) == ".")
		{
			pos++;
			while (pos < text.Length() && VyshkaJson.IsDigit(text.Get(pos)))
				pos++;
		}
		if (pos >= text.Length())
			return false;

		int offsetSeconds = 0;
		string zone = text.Get(pos);
		if (zone == "Z" || zone == "z")
		{
			pos++;
		}
		else if (zone == "+" || zone == "-")
		{
			if (pos + 6 > text.Length())
				return false;
			int offsetHour = ReadInt(text, pos + 1, 2);
			int offsetMinute = ReadInt(text, pos + 4, 2);
			if (offsetHour < 0 || offsetMinute < 0 || text.Get(pos + 3) != ":")
				return false;
			offsetSeconds = offsetHour * 3600 + offsetMinute * 60;
			if (zone == "-")
				offsetSeconds = -offsetSeconds;
			pos += 6;
		}
		else
		{
			return false;
		}
		if (pos != text.Length())
			return false;

		epoch = DaysFromCivil(year, month, day) * 86400 + hour * 3600 + minute * 60 + second - offsetSeconds;
		return true;
	}

	// ReadInt reads exactly count decimal digits at start, or returns -1.
	static int ReadInt(string text, int start, int count)
	{
		if (start + count > text.Length())
			return -1;
		int value = 0;
		for (int i = 0; i < count; i++)
		{
			string c = text.Get(start + i);
			if (!VyshkaJson.IsDigit(c))
				return -1;
			value = value * 10 + (c.ToAscii() - 48);
		}
		return value;
	}
}

// VyshkaIds mints envelope ids. Spec section 4 wants an id unique per message
// on this server across every session, stable across retransmissions, and
// opaque to the receiver; ULIDs are recommended but not required. The engine
// has no ULID encoder and no high-resolution clock, so an id here is the UTC
// second the process first minted one, a random tag drawn at that moment, and
// a counter: unique across restarts by the timestamp and tag, unique within a
// run by the counter.
class VyshkaIds
{
	protected static string s_Prefix = "";
	protected static int s_Counter = 0;

	static string Next()
	{
		if (s_Prefix == "")
			s_Prefix = "dz-" + VyshkaClock.NowCompact() + "-" + RandomHex(8);
		s_Counter++;
		return s_Prefix + "-" + s_Counter.ToString();
	}

	static string RandomHex(int length)
	{
		string digits = "0123456789abcdef";
		string result = "";
		for (int i = 0; i < length; i++)
			result += digits.Get(Math.RandomInt(0, 16));
		return result;
	}
}
