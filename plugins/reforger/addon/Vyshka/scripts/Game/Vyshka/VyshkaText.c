// Vyshka Reforger plugin: text helpers the DayZ engine has and Reforger does
// not. The protocol counts lengths in code points (spec section 6.1), and a
// cut must not split a multi-byte character.

class VyshkaText
{
	// IsContinuation says whether the byte at index is a UTF-8 continuation
	// byte (10xxxxxx).
	static bool IsContinuation(string text, int index)
	{
		int b = text.ToAscii(index);
		if (b < 0)
			b += 256;
		return b >= 128 && b < 192;
	}

	// LengthUtf8 counts the code points in text.
	static int LengthUtf8(string text)
	{
		int bytes = text.Length();
		int count = 0;
		for (int i = 0; i < bytes; i++)
		{
			if (!IsContinuation(text, i))
				count++;
		}
		return count;
	}

	// PrefixUtf8 returns the first maxLength code points of text. It builds
	// the result by concatenation, since Substring caps at 8191 characters
	// (spikes/reforger-server-script-limits, run 3).
	static string PrefixUtf8(string text, int maxLength)
	{
		int bytes = text.Length();
		int count = 0;
		int end = bytes;
		for (int i = 0; i < bytes; i++)
		{
			if (IsContinuation(text, i))
				continue;
			if (count == maxLength)
			{
				end = i;
				break;
			}
			count++;
		}
		if (end == bytes)
			return text;
		string result = "";
		int at = 0;
		while (at < end)
		{
			int take = end - at;
			if (take > 4096)
				take = 4096;
			result += text.Substring(at, take);
			at += take;
		}
		return result;
	}

	// ValidName says whether name is dot-separated segments of letters,
	// digits, underscore and hyphen, at most maxLength bytes (spec section
	// 12.1).
	static bool ValidName(string name, int maxLength)
	{
		int length = name.Length();
		if (length == 0 || length > maxLength)
			return false;
		int segment = 0;
		for (int i = 0; i < length; i++)
		{
			string c = name.Get(i);
			if (c == ".")
			{
				if (segment == 0)
					return false;
				segment = 0;
				continue;
			}
			if (!IsNameChar(c))
				return false;
			segment++;
		}
		return segment > 0;
	}

	static bool IsNameChar(string c)
	{
		if (c == "_" || c == "-")
			return true;
		int code = c.ToAscii();
		return (code >= 48 && code <= 57) || (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
	}
}
