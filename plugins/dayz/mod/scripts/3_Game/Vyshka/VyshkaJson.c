// Vyshka DayZ plugin: a small generic JSON tree, parser, and writer.
//
// The engine ships a typed JSON serializer, but every envelope body this
// plugin receives is shaped by its type, and the protocol obliges a receiver
// to accept bodies it does not understand (spec section 4: unknown types are
// acked and ignored; section 2.1: unknown fields are ignored). A typed
// decoder that failed on an unexpected shape would refuse the whole poll
// response, including the envelopes travelling with it, and the hub would
// resend the same batch forever: a wedged session over one field. So the
// plugin reads JSON into a generic tree and picks out what it needs.
//
// Numbers are kept as their original text beside the parsed value, so a
// value the plugin never touches is re-emitted exactly. Strings are byte
// strings in UTF-8, which is what the engine's string type holds.

enum VyshkaJsonKind
{
	NULL_VALUE,
	BOOL_VALUE,
	NUMBER_VALUE,
	STRING_VALUE,
	ARRAY_VALUE,
	OBJECT_VALUE
}

class VyshkaJsonValue : Managed
{
	int m_Kind;
	bool m_Bool;
	float m_Number;
	int m_Int;          // the integer reading of a number, clamped into int range
	bool m_IsInteger;   // true when the number text had no fraction or exponent
	string m_Text;      // number: original text; string: decoded content

	ref array<ref VyshkaJsonValue> m_Items;   // array elements
	ref array<string> m_Keys;                 // object keys, in insertion order
	ref array<ref VyshkaJsonValue> m_Values;  // object values, parallel to m_Keys

	// ---- constructors ----

	static VyshkaJsonValue NewNull()
	{
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.NULL_VALUE;
		return v;
	}

	static VyshkaJsonValue NewBool(bool value)
	{
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.BOOL_VALUE;
		v.m_Bool = value;
		return v;
	}

	static VyshkaJsonValue NewInt(int value)
	{
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.NUMBER_VALUE;
		v.m_Int = value;
		v.m_Number = value;
		v.m_IsInteger = true;
		v.m_Text = value.ToString();
		return v;
	}

	// The magnitude FormatFloat can render: the whole part has to fit the
	// engine's 32-bit int, and nothing measured in metres on a game map comes
	// anywhere near it.
	static const float FLOAT_MAGNITUDE_MAX = 2000000000.0;

	// NewFloat carries a game measurement (a position, a distance) as a JSON
	// number with two decimals, or returns null for a value it cannot render
	// as one (NaN, infinite, or beyond FLOAT_MAGNITUDE_MAX), so a caller can
	// leave the field out rather than emit something a hub would reject. The
	// text is built from integer arithmetic rather than the engine's float
	// formatting, whose output form (exponent notation, locale) is not
	// specified to be a JSON number.
	static VyshkaJsonValue NewFloat(float value)
	{
		// A NaN is the one float that is not equal to itself.
		if (!(value == value) || Math.AbsFloat(value) > FLOAT_MAGNITUDE_MAX)
			return null;
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.NUMBER_VALUE;
		v.m_Number = value;
		v.m_Int = (int)value;
		v.m_IsInteger = false;
		v.m_Text = FormatFloat(value);
		return v;
	}

	// FormatFloat renders a float as [-]whole.hh, rounded to the hundredth.
	static string FormatFloat(float value)
	{
		bool negative = value < 0;
		if (negative)
			value = -value;
		int whole = (int)value;
		int hundredths = (int)Math.Round((value - whole) * 100);
		if (hundredths >= 100)
		{
			whole += 1;
			hundredths -= 100;
		}
		string text = whole.ToString() + ".";
		if (hundredths < 10)
			text += "0";
		text += hundredths.ToString();
		if (negative && (whole > 0 || hundredths > 0))
			text = "-" + text;
		return text;
	}

	static VyshkaJsonValue NewString(string value)
	{
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.STRING_VALUE;
		v.m_Text = value;
		return v;
	}

	static VyshkaJsonValue NewArray()
	{
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.ARRAY_VALUE;
		v.m_Items = new array<ref VyshkaJsonValue>;
		return v;
	}

	static VyshkaJsonValue NewObject()
	{
		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.OBJECT_VALUE;
		v.m_Keys = new array<string>;
		v.m_Values = new array<ref VyshkaJsonValue>;
		return v;
	}

	// ---- kind tests ----

	bool IsNull()   { return m_Kind == VyshkaJsonKind.NULL_VALUE; }
	bool IsBool()   { return m_Kind == VyshkaJsonKind.BOOL_VALUE; }
	bool IsNumber() { return m_Kind == VyshkaJsonKind.NUMBER_VALUE; }
	bool IsString() { return m_Kind == VyshkaJsonKind.STRING_VALUE; }
	bool IsArray()  { return m_Kind == VyshkaJsonKind.ARRAY_VALUE; }
	bool IsObject() { return m_Kind == VyshkaJsonKind.OBJECT_VALUE; }

	// ---- array access ----

	int Count()
	{
		if (m_Kind == VyshkaJsonKind.ARRAY_VALUE)
			return m_Items.Count();
		if (m_Kind == VyshkaJsonKind.OBJECT_VALUE)
			return m_Keys.Count();
		return 0;
	}

	VyshkaJsonValue At(int index)
	{
		if (m_Kind != VyshkaJsonKind.ARRAY_VALUE || index < 0 || index >= m_Items.Count())
			return null;
		return m_Items.Get(index);
	}

	VyshkaJsonValue Add(VyshkaJsonValue value)
	{
		if (m_Kind == VyshkaJsonKind.ARRAY_VALUE)
			m_Items.Insert(value);
		return this;
	}

	// ---- object access ----

	VyshkaJsonValue Get(string key)
	{
		if (m_Kind != VyshkaJsonKind.OBJECT_VALUE)
			return null;
		int index = m_Keys.Find(key);
		if (index < 0)
			return null;
		return m_Values.Get(index);
	}

	bool Has(string key)
	{
		return Get(key) != null;
	}

	VyshkaJsonValue Set(string key, VyshkaJsonValue value)
	{
		if (m_Kind != VyshkaJsonKind.OBJECT_VALUE)
			return this;
		int index = m_Keys.Find(key);
		if (index < 0)
		{
			m_Keys.Insert(key);
			m_Values.Insert(value);
		}
		else
		{
			m_Values.Set(index, value);
		}
		return this;
	}

	string KeyAt(int index)
	{
		if (m_Kind != VyshkaJsonKind.OBJECT_VALUE || index < 0 || index >= m_Keys.Count())
			return "";
		return m_Keys.Get(index);
	}

	VyshkaJsonValue ValueAt(int index)
	{
		if (m_Kind != VyshkaJsonKind.OBJECT_VALUE || index < 0 || index >= m_Values.Count())
			return null;
		return m_Values.Get(index);
	}

	// ---- typed convenience readers, each with a fallback ----

	string GetString(string key, string fallback = "")
	{
		VyshkaJsonValue v = Get(key);
		if (!v || !v.IsString())
			return fallback;
		return v.m_Text;
	}

	int GetInt(string key, int fallback = 0)
	{
		VyshkaJsonValue v = Get(key);
		if (!v || !v.IsNumber())
			return fallback;
		return v.m_Int;
	}

	float GetFloat(string key, float fallback = 0)
	{
		VyshkaJsonValue v = Get(key);
		if (!v || !v.IsNumber())
			return fallback;
		return v.m_Number;
	}

	bool GetBool(string key, bool fallback = false)
	{
		VyshkaJsonValue v = Get(key);
		if (!v || !v.IsBool())
			return fallback;
		return v.m_Bool;
	}

	// ---- serialization ----

	string Serialize()
	{
		string result = "";
		WriteTo(result);
		return result;
	}

	void WriteTo(inout string result)
	{
		switch (m_Kind)
		{
			case VyshkaJsonKind.NULL_VALUE:
				result += "null";
				break;
			case VyshkaJsonKind.BOOL_VALUE:
				if (m_Bool)
					result += "true";
				else
					result += "false";
				break;
			case VyshkaJsonKind.NUMBER_VALUE:
				result += m_Text;
				break;
			case VyshkaJsonKind.STRING_VALUE:
				result += VyshkaJson.Quote(m_Text);
				break;
			case VyshkaJsonKind.ARRAY_VALUE:
				result += "[";
				for (int i = 0; i < m_Items.Count(); i++)
				{
					if (i > 0)
						result += ",";
					m_Items.Get(i).WriteTo(result);
				}
				result += "]";
				break;
			case VyshkaJsonKind.OBJECT_VALUE:
				result += "{";
				for (int k = 0; k < m_Keys.Count(); k++)
				{
					if (k > 0)
						result += ",";
					result += VyshkaJson.Quote(m_Keys.Get(k));
					result += ":";
					m_Values.Get(k).WriteTo(result);
				}
				result += "}";
				break;
		}
	}
}

// VyshkaJson holds the parser and the string quoting helper. Parse returns
// null on malformed input and never throws: a bad response body is a
// transport failure to retry, not a reason to crash the game server.
class VyshkaJson
{
	protected string m_Input;
	protected int m_Pos;
	protected int m_Length;
	protected int m_Depth;
	protected bool m_Failed;

	static const int MAX_DEPTH = 64;

	static VyshkaJsonValue Parse(string input)
	{
		VyshkaJson parser = new VyshkaJson();
		parser.m_Input = input;
		parser.m_Pos = 0;
		parser.m_Length = input.Length();
		parser.m_Depth = 0;
		parser.m_Failed = false;

		parser.SkipWhitespace();
		VyshkaJsonValue value = parser.ParseValue();
		if (!value || parser.m_Failed)
			return null;
		parser.SkipWhitespace();
		if (parser.m_Pos != parser.m_Length)
			return null;
		return value;
	}

	// Quote escapes a string for JSON output. Bytes above 0x7F pass through
	// untouched (the engine's strings are UTF-8 already).
	static string Quote(string text)
	{
		string result = "\"";
		int length = text.Length();
		int runStart = 0;
		for (int i = 0; i < length; i++)
		{
			string c = text.Get(i);
			string escaped = "";
			// The engine's script parser cannot scan a literal that combines
			// a backslash escape with a second escape, so the backslash is
			// built from its byte value rather than written as "\\".
			if (c == "\"")
				escaped = Backslash() + "\"";
			else if (c == Backslash())
				escaped = Backslash() + Backslash();
			else
			{
				int code = c.ToAscii();
				if (code >= 0 && code < 32)
				{
					if (code == 10)
						escaped = Backslash() + "n";
					else if (code == 13)
						escaped = Backslash() + "r";
					else if (code == 9)
						escaped = Backslash() + "t";
					else if (code == 8)
						escaped = Backslash() + "b";
					else if (code == 12)
						escaped = Backslash() + "f";
					else
						escaped = Backslash() + "u00" + HexByte(code);
				}
			}
			if (escaped != "")
			{
				if (i > runStart)
					result += text.Substring(runStart, i - runStart);
				result += escaped;
				runStart = i + 1;
			}
		}
		if (length > runStart)
			result += text.Substring(runStart, length - runStart);
		result += "\"";
		return result;
	}

	// Backslash is the one-byte string "\", built from its byte value.
	static string Backslash()
	{
		int code = 92;
		return code.AsciiToString();
	}

	static string HexByte(int value)
	{
		string digits = "0123456789abcdef";
		int high = (value / 16) & 15;
		int low = value & 15;
		return digits.Get(high) + digits.Get(low);
	}

	// ---- parser internals ----

	protected string Peek()
	{
		if (m_Pos >= m_Length)
			return "";
		return m_Input.Get(m_Pos);
	}

	protected void SkipWhitespace()
	{
		while (m_Pos < m_Length)
		{
			string c = m_Input.Get(m_Pos);
			if (c == " " || c == "\t" || c == "\n" || c == "\r")
				m_Pos++;
			else
				break;
		}
	}

	protected bool Expect(string literal)
	{
		int length = literal.Length();
		if (m_Pos + length > m_Length)
			return false;
		if (m_Input.Substring(m_Pos, length) != literal)
			return false;
		m_Pos += length;
		return true;
	}

	protected VyshkaJsonValue ParseValue()
	{
		if (m_Failed)
			return null;
		string c = Peek();
		if (c == "")
		{
			m_Failed = true;
			return null;
		}
		if (c == "{")
			return ParseObject();
		if (c == "[")
			return ParseArray();
		if (c == "\"")
		{
			string text;
			if (!ParseString(text))
			{
				m_Failed = true;
				return null;
			}
			return VyshkaJsonValue.NewString(text);
		}
		if (c == "t")
		{
			if (Expect("true"))
				return VyshkaJsonValue.NewBool(true);
			m_Failed = true;
			return null;
		}
		if (c == "f")
		{
			if (Expect("false"))
				return VyshkaJsonValue.NewBool(false);
			m_Failed = true;
			return null;
		}
		if (c == "n")
		{
			if (Expect("null"))
				return VyshkaJsonValue.NewNull();
			m_Failed = true;
			return null;
		}
		if (c == "-" || IsDigit(c))
			return ParseNumber();
		m_Failed = true;
		return null;
	}

	protected VyshkaJsonValue ParseObject()
	{
		if (m_Depth >= MAX_DEPTH)
		{
			m_Failed = true;
			return null;
		}
		m_Depth++;
		m_Pos++; // "{"
		VyshkaJsonValue obj = VyshkaJsonValue.NewObject();
		SkipWhitespace();
		if (Peek() == "}")
		{
			m_Pos++;
			m_Depth--;
			return obj;
		}
		while (true)
		{
			SkipWhitespace();
			if (Peek() != "\"")
			{
				m_Failed = true;
				return null;
			}
			string key;
			if (!ParseString(key))
			{
				m_Failed = true;
				return null;
			}
			SkipWhitespace();
			if (Peek() != ":")
			{
				m_Failed = true;
				return null;
			}
			m_Pos++;
			SkipWhitespace();
			VyshkaJsonValue value = ParseValue();
			if (!value || m_Failed)
			{
				m_Failed = true;
				return null;
			}
			obj.Set(key, value);
			SkipWhitespace();
			string c = Peek();
			if (c == ",")
			{
				m_Pos++;
				continue;
			}
			if (c == "}")
			{
				m_Pos++;
				m_Depth--;
				return obj;
			}
			m_Failed = true;
			return null;
		}
		return null;
	}

	protected VyshkaJsonValue ParseArray()
	{
		if (m_Depth >= MAX_DEPTH)
		{
			m_Failed = true;
			return null;
		}
		m_Depth++;
		m_Pos++; // "["
		VyshkaJsonValue list = VyshkaJsonValue.NewArray();
		SkipWhitespace();
		if (Peek() == "]")
		{
			m_Pos++;
			m_Depth--;
			return list;
		}
		while (true)
		{
			SkipWhitespace();
			VyshkaJsonValue value = ParseValue();
			if (!value || m_Failed)
			{
				m_Failed = true;
				return null;
			}
			list.Add(value);
			SkipWhitespace();
			string c = Peek();
			if (c == ",")
			{
				m_Pos++;
				continue;
			}
			if (c == "]")
			{
				m_Pos++;
				m_Depth--;
				return list;
			}
			m_Failed = true;
			return null;
		}
		return null;
	}

	// ParseString decodes a quoted string starting at the opening quote.
	protected bool ParseString(out string text)
	{
		m_Pos++; // opening quote
		text = "";
		int runStart = m_Pos;
		while (m_Pos < m_Length)
		{
			string c = m_Input.Get(m_Pos);
			if (c == "\"")
			{
				if (m_Pos > runStart)
					text += m_Input.Substring(runStart, m_Pos - runStart);
				m_Pos++;
				return true;
			}
			if (c == "\\")
			{
				if (m_Pos > runStart)
					text += m_Input.Substring(runStart, m_Pos - runStart);
				m_Pos++;
				if (m_Pos >= m_Length)
					return false;
				string e = m_Input.Get(m_Pos);
				m_Pos++;
				if (e == "\"")
					text += "\"";
				else if (e == "\\")
					text += "\\";
				else if (e == "/")
					text += "/";
				else if (e == "n")
					text += "\n";
				else if (e == "r")
					text += "\r";
				else if (e == "t")
					text += "\t";
				else if (e == "b")
				{
					int bs = 8;
					text += bs.AsciiToString();
				}
				else if (e == "f")
				{
					int ff = 12;
					text += ff.AsciiToString();
				}
				else if (e == "u")
				{
					int codePoint;
					if (!ParseHex4(codePoint))
						return false;
					// A UTF-16 surrogate pair encodes one code point above
					// the BMP; a lone surrogate is replaced, not rejected.
					if (codePoint >= 0xD800 && codePoint <= 0xDBFF)
					{
						int lowSurrogate = -1;
						if (m_Pos + 1 < m_Length && m_Input.Get(m_Pos) == "\\" && m_Input.Get(m_Pos + 1) == "u")
						{
							int save = m_Pos;
							m_Pos += 2;
							if (!ParseHex4(lowSurrogate) || lowSurrogate < 0xDC00 || lowSurrogate > 0xDFFF)
							{
								m_Pos = save;
								lowSurrogate = -1;
							}
						}
						if (lowSurrogate >= 0)
							codePoint = 0x10000 + ((codePoint - 0xD800) * 0x400) + (lowSurrogate - 0xDC00);
						else
							codePoint = 0xFFFD;
					}
					else if (codePoint >= 0xDC00 && codePoint <= 0xDFFF)
					{
						codePoint = 0xFFFD;
					}
					text += EncodeUtf8(codePoint);
				}
				else
					return false;
				runStart = m_Pos;
				continue;
			}
			m_Pos++;
		}
		return false;
	}

	protected bool ParseHex4(out int value)
	{
		if (m_Pos + 4 > m_Length)
			return false;
		value = 0;
		for (int i = 0; i < 4; i++)
		{
			int digit = HexDigit(m_Input.Get(m_Pos + i));
			if (digit < 0)
				return false;
			value = value * 16 + digit;
		}
		m_Pos += 4;
		return true;
	}

	static int HexDigit(string c)
	{
		int code = c.ToAscii();
		if (code >= 48 && code <= 57)
			return code - 48;
		if (code >= 97 && code <= 102)
			return code - 87;
		if (code >= 65 && code <= 70)
			return code - 55;
		return -1;
	}

	// EncodeUtf8 writes one code point as UTF-8 bytes. AsciiToString on a
	// value above 127 is relied on to yield that single byte.
	static string EncodeUtf8(int codePoint)
	{
		int b;
		if (codePoint < 0x80)
		{
			b = codePoint;
			return b.AsciiToString();
		}
		string result = "";
		if (codePoint < 0x800)
		{
			b = 0xC0 | (codePoint >> 6);
			result += b.AsciiToString();
			b = 0x80 | (codePoint & 0x3F);
			result += b.AsciiToString();
			return result;
		}
		if (codePoint < 0x10000)
		{
			b = 0xE0 | (codePoint >> 12);
			result += b.AsciiToString();
			b = 0x80 | ((codePoint >> 6) & 0x3F);
			result += b.AsciiToString();
			b = 0x80 | (codePoint & 0x3F);
			result += b.AsciiToString();
			return result;
		}
		b = 0xF0 | (codePoint >> 18);
		result += b.AsciiToString();
		b = 0x80 | ((codePoint >> 12) & 0x3F);
		result += b.AsciiToString();
		b = 0x80 | ((codePoint >> 6) & 0x3F);
		result += b.AsciiToString();
		b = 0x80 | (codePoint & 0x3F);
		result += b.AsciiToString();
		return result;
	}

	static bool IsDigit(string c)
	{
		int code = c.ToAscii();
		return code >= 48 && code <= 57;
	}

	protected VyshkaJsonValue ParseNumber()
	{
		int start = m_Pos;
		bool negative = false;
		bool isInteger = true;
		if (Peek() == "-")
		{
			negative = true;
			m_Pos++;
		}
		if (!IsDigit(Peek()))
		{
			m_Failed = true;
			return null;
		}
		// The integer reading saturates rather than wrapping, so a value
		// outside 32-bit range is still a number, just not one the plugin
		// can use as a sequence number.
		int magnitude = 0;
		bool overflow = false;
		bool firstIsZero = (Peek() == "0");
		int digitCount = 0;
		while (IsDigit(Peek()))
		{
			int digit = Peek().ToAscii() - 48;
			if (magnitude > 214748364 || (magnitude == 214748364 && digit > 7))
				overflow = true;
			else
				magnitude = magnitude * 10 + digit;
			digitCount++;
			m_Pos++;
		}
		// JSON forbids a leading zero (0 is fine, 01 is not). Rejecting it keeps
		// a corrupt response from parsing into a plausible number, such as an
		// ack that would delete an envelope the hub never acknowledged.
		if (firstIsZero && digitCount > 1)
		{
			m_Failed = true;
			return null;
		}
		if (Peek() == ".")
		{
			isInteger = false;
			m_Pos++;
			if (!IsDigit(Peek()))
			{
				m_Failed = true;
				return null;
			}
			while (IsDigit(Peek()))
				m_Pos++;
		}
		string exp = Peek();
		if (exp == "e" || exp == "E")
		{
			isInteger = false;
			m_Pos++;
			string sign = Peek();
			if (sign == "+" || sign == "-")
				m_Pos++;
			if (!IsDigit(Peek()))
			{
				m_Failed = true;
				return null;
			}
			while (IsDigit(Peek()))
				m_Pos++;
		}

		VyshkaJsonValue v = new VyshkaJsonValue();
		v.m_Kind = VyshkaJsonKind.NUMBER_VALUE;
		v.m_Text = m_Input.Substring(start, m_Pos - start);
		v.m_IsInteger = isInteger && !overflow;
		v.m_Number = v.m_Text.ToFloat();
		if (overflow)
			magnitude = 2147483647;
		if (negative)
			v.m_Int = -magnitude;
		else
			v.m_Int = magnitude;
		if (!isInteger)
			v.m_Int = (int)v.m_Number;
		return v;
	}
}
