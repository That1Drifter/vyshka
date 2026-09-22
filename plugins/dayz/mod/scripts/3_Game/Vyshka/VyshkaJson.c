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
//
// What the engine charges for strings shapes everything below (measured in
// spikes/dayz-bans-pull-size on DayZ 1.29, 2026-09-21; issue #108):
// string.Get(i) and Substring cost time proportional to the length of the
// string they read from, whatever the index, about 0.2 us per KiB per call;
// += pays for the string's current length; and Substring returns at most
// 8 191 characters, silently. A parser that reads an n-byte input one
// character at a time, and a serializer that appends every token to one
// result, are therefore quadratic in n (184 ms for 24 KB, 353 s for
// 1.2 MB, as the plugin's first parser was). So the parser reads through a
// VyshkaTextCursor, which cuts the input once into windows and reads every
// character from a small one, and the serializer collects pieces in a
// VyshkaJsonWriter and joins them once.

enum VyshkaJsonKind
{
	NULL_VALUE,
	BOOL_VALUE,
	NUMBER_VALUE,
	STRING_VALUE,
	ARRAY_VALUE,
	OBJECT_VALUE
}

// VyshkaTextCursor reads characters and slices out of a large string, or
// out of a list of segments (a file's lines), at a cost that does not grow
// with the whole. The source is cut into outer windows of at most OUTER
// characters (the most one Substring returns), each outer window into inner
// pieces of INNER, and every read is a Get on the inner piece. Cutting an
// outer window costs one Substring on the whole source, and there are
// n / OUTER of them, which is the residual quadratic term: about 35 ms for
// 1.2 MB, about 27 s for the 32 MiB the HTTP client delivered in the spike,
// so a document of the latter size is still not one to parse in a frame. In
// segment mode there is no such term: an outer window is a segment, and the
// segments are the lines a file reader produced, each bounded by the
// reader's own limit.
//
// Reads run forward with occasional short backtracks (the parser looking
// past an escape), so the cursor keeps one window of each level and moves
// it when a read falls outside; a read far behind simply cuts again.
class VyshkaTextCursor
{
	static const int OUTER = 8191;
	static const int INNER = 256;
	static const int SLICE_GROUP = 4096;

	protected string m_Source;                 // string mode
	protected ref array<string> m_Segments;    // segment mode: the source is these, concatenated verbatim
	protected ref array<int> m_SegmentStarts;  // offset of each segment in the whole
	protected int m_Length;
	protected string m_Outer;
	protected int m_OuterStart;
	protected int m_OuterEnd;
	protected string m_Inner;
	protected int m_InnerStart;
	protected int m_InnerEnd;
	protected int m_SegmentIndex;

	static VyshkaTextCursor OfString(string source)
	{
		VyshkaTextCursor cursor = new VyshkaTextCursor();
		cursor.m_Source = source;
		cursor.m_Length = source.Length();
		return cursor;
	}

	// OfSegments reads the segments as one text, joined by nothing: a file
	// reader that wants its lines separated hands them over with the
	// newline on each.
	static VyshkaTextCursor OfSegments(array<string> segments)
	{
		VyshkaTextCursor cursor = new VyshkaTextCursor();
		cursor.m_Segments = segments;
		cursor.m_SegmentStarts = new array<int>;
		int total = 0;
		for (int i = 0; i < segments.Count(); i++)
		{
			cursor.m_SegmentStarts.Insert(total);
			string segment = segments.Get(i);
			total += segment.Length();
		}
		cursor.m_Length = total;
		return cursor;
	}

	void VyshkaTextCursor()
	{
		m_Length = 0;
		m_OuterStart = 0;
		m_OuterEnd = 0;
		m_InnerStart = 0;
		m_InnerEnd = 0;
		m_SegmentIndex = 0;
	}

	int Length()
	{
		return m_Length;
	}

	// CharAt is the character at pos, or "" past either end.
	string CharAt(int pos)
	{
		if (pos < m_InnerStart || pos >= m_InnerEnd)
		{
			if (!Seek(pos))
				return "";
		}
		return m_Inner.Get(pos - m_InnerStart);
	}

	// WindowEnd is where the inner piece the last read came from ends, so a
	// caller copying runs can cut them at a piece boundary and never ask for
	// a slice that spans two.
	int WindowEnd()
	{
		return m_InnerEnd;
	}

	// Slice copies length characters from start, whatever windows they lie
	// in; a slice past the end is cut at the end. Each piece copied is at
	// most INNER long, so no Substring is ever asked for more than it can
	// return, and a run longer than the cap is assembled whole: in groups
	// of SLICE_GROUP joined once, so a long slice costs its length rather
	// than its square.
	string Slice(int start, int length)
	{
		string result = "";
		int resultLength = 0;
		array<string> groups = null;
		int end = start + length;
		if (end > m_Length)
			end = m_Length;
		int pos = start;
		while (pos < end)
		{
			if (pos < m_InnerStart || pos >= m_InnerEnd)
			{
				if (!Seek(pos))
					break;
			}
			int take = m_InnerEnd - pos;
			if (pos + take > end)
				take = end - pos;
			if (pos == m_InnerStart && take == m_InnerEnd - m_InnerStart)
				result += m_Inner;
			else
				result += m_Inner.Substring(pos - m_InnerStart, take);
			resultLength += take;
			pos += take;
			if (resultLength >= SLICE_GROUP && pos < end)
			{
				if (!groups)
					groups = new array<string>;
				groups.Insert(result);
				result = "";
				resultLength = 0;
			}
		}
		if (!groups)
			return result;
		if (resultLength > 0)
			groups.Insert(result);
		return VyshkaJsonWriter.JoinPieces(groups);
	}

	// Seek moves the inner piece (and the outer window when needed) over
	// pos; false when pos is outside the text.
	protected bool Seek(int pos)
	{
		if (pos < 0 || pos >= m_Length)
			return false;
		if (pos < m_OuterStart || pos >= m_OuterEnd)
			SeekOuter(pos);
		int offset = pos - m_OuterStart;
		int innerStart = m_OuterStart + (offset / INNER) * INNER;
		int innerEnd = innerStart + INNER;
		if (innerEnd > m_OuterEnd)
			innerEnd = m_OuterEnd;
		if (innerStart == m_OuterStart && innerEnd == m_OuterEnd)
			m_Inner = m_Outer;
		else
			m_Inner = m_Outer.Substring(innerStart - m_OuterStart, innerEnd - innerStart);
		m_InnerStart = innerStart;
		m_InnerEnd = innerEnd;
		return true;
	}

	protected void SeekOuter(int pos)
	{
		if (!m_Segments)
		{
			int start = (pos / OUTER) * OUTER;
			int end = start + OUTER;
			if (end > m_Length)
				end = m_Length;
			if (start == 0 && end == m_Length)
				m_Outer = m_Source;
			else
				m_Outer = m_Source.Substring(start, end - start);
			m_OuterStart = start;
			m_OuterEnd = end;
			return;
		}
		int count = m_Segments.Count();
		while (m_SegmentIndex > 0 && pos < m_SegmentStarts.Get(m_SegmentIndex))
			m_SegmentIndex--;
		while (m_SegmentIndex + 1 < count && pos >= m_SegmentStarts.Get(m_SegmentIndex + 1))
			m_SegmentIndex++;
		m_Outer = m_Segments.Get(m_SegmentIndex);
		m_OuterStart = m_SegmentStarts.Get(m_SegmentIndex);
		if (m_SegmentIndex + 1 < count)
			m_OuterEnd = m_SegmentStarts.Get(m_SegmentIndex + 1);
		else
			m_OuterEnd = m_Length;
	}
}

// VyshkaJsonWriter collects the pieces of a serialization and joins them
// once: appends go to a small buffer, the buffer is set aside as a chunk
// when it passes CHUNK, and Join assembles the chunks in two levels so no
// append ever pays for the whole result. WriteFile hands the chunks to the
// file one by one and never assembles them at all.
//
// In lines mode every array element and object member starts a line of its
// own, indented one tab per depth. A newline between JSON tokens is
// whitespace, so the text stays valid for any reader, and no line is longer
// than its longest token, which is what a file read back through the
// engine's line reader needs (VyshkaFiles.LINE_MAX). The longest line is
// tracked so a writer can refuse a file that would not read back, and so
// is the deepest nesting, since the parser reads no deeper than
// VyshkaJson.MAX_DEPTH.
//
// A file writer may also ask for long strings to be chunked: a string
// value of LONG_STRING_MIN characters or more then goes out as an object
// with the one member LONG_STRING_KEY holding an array of pieces, one per
// line, and the file reader folds it back (VyshkaJsonValue.Unchunk). A
// JSON string cannot be broken across lines any other way, and without
// this a legal result carrying one long string could not be persisted.
// The threshold is what keeps a line under VyshkaFiles.LINE_MAX whatever
// the string holds: a character escapes to at most six bytes, so a value
// under the threshold is under 49 152 bytes quoted, and so is a piece.
// So that a genuine document can never be mistaken for the marker, a key
// beginning with KEY_PREFIX is written with one more "$" in front of it
// and read back without (EscapeKey, UnescapeKey); the marker itself is
// written only by WriteChunked.
class VyshkaJsonWriter
{
	static const int CHUNK = 4096;
	static const int JOIN_GROUP = 16;
	static const string KEY_PREFIX = "$vyshka.";
	static const string LONG_STRING_KEY = "$vyshka.longString";
	static const int LONG_STRING_MIN = 8192;
	static const int LONG_STRING_PIECE = 4096;
	// A key longer than this goes on a line of its own, its value on the
	// next: quoted, a key of at most LONG_STRING_MIN characters and a value
	// under the chunk threshold each fit a line, and only together could
	// they pass it.
	static const int LONG_KEY_ALONE = 1024;

	// EscapeKey is the file form of an object key: one more "$" in front
	// of a key that is one or more "$" followed by "vyshka.", so that on
	// disk a key with exactly one "$" is the marker and nothing else, and
	// every escaped key comes back as it was.
	static string EscapeKey(string key)
	{
		if (DollarsBeforePrefix(key) >= 1)
			return "$" + key;
		return key;
	}

	// UnescapeKey undoes EscapeKey: a key of two or more "$" followed by
	// "vyshka." loses one. The rest is copied through a cursor, since one
	// Substring returns at most 8 191 characters.
	static string UnescapeKey(string key)
	{
		if (DollarsBeforePrefix(key) >= 2)
		{
			VyshkaTextCursor cursor = VyshkaTextCursor.OfString(key);
			return cursor.Slice(1, key.Length() - 1);
		}
		return key;
	}

	// DollarsBeforePrefix counts the leading "$" of a key whose rest begins
	// with "vyshka."; 0 for any other key. The key is read through a cursor
	// so a long key costs its length, not its square.
	static int DollarsBeforePrefix(string key)
	{
		int length = key.Length();
		if (length < 2 || key.Get(0) != "$")
			return 0;
		VyshkaTextCursor cursor = VyshkaTextCursor.OfString(key);
		int dollars = 0;
		while (dollars < length && cursor.CharAt(dollars) == "$")
			dollars++;
		string rest = KEY_PREFIX.Substring(1, KEY_PREFIX.Length() - 1);   // "vyshka."
		int restLength = rest.Length();
		if (length - dollars < restLength)
			return 0;
		if (cursor.Slice(dollars, restLength) != rest)
			return 0;
		return dollars;
	}

	protected ref array<string> m_Chunks;
	protected string m_Buffer;
	protected int m_BufferLength;
	protected bool m_Lines;
	protected bool m_ChunkStrings;
	protected int m_Depth;
	protected int m_MaxDepth;
	protected int m_LineLength;
	protected int m_LongestLine;
	protected int m_Length;

	void VyshkaJsonWriter(bool lines)
	{
		m_Chunks = new array<string>;
		m_Buffer = "";
		m_BufferLength = 0;
		m_Lines = lines;
		m_ChunkStrings = false;
		m_Depth = 0;
		m_MaxDepth = 0;
		m_LineLength = 0;
		m_LongestLine = 0;
		m_Length = 0;
	}

	// ChunkStrings turns long-string chunking on (file writers only).
	void ChunkStrings(bool on)
	{
		m_ChunkStrings = on;
	}

	// ChunksStrings says whether this writer chunks long strings and
	// escapes reserved keys (a file writer).
	bool ChunksStrings()
	{
		return m_ChunkStrings;
	}

	// ChunksString says whether a string of the given length goes out in
	// pieces under this writer.
	bool ChunksString(int length)
	{
		return m_ChunkStrings && length >= LONG_STRING_MIN;
	}

	// WriteChunked writes a long string as the chunk object. A piece ends
	// on a character boundary: a UTF-8 sequence is never split between two
	// pieces, so each piece is text a strict reader accepts, and the
	// bytes joined are the original.
	void WriteChunked(string text)
	{
		Open("{");
		Newline();
		VyshkaJson.QuoteTo(this, LONG_STRING_KEY);
		Append(":");
		Open("[");
		VyshkaTextCursor cursor = VyshkaTextCursor.OfString(text);
		int length = cursor.Length();
		int pos = 0;
		int pieces = 0;
		while (pos < length)
		{
			int take = LONG_STRING_PIECE;
			if (pos + take > length)
				take = length - pos;
			// Back off while the byte after the cut continues a sequence
			// (10xxxxxx); a run of continuation bytes longer than a
			// sequence can be is not UTF-8, and is cut where it stands.
			int backed = 0;
			while (pos + take < length && backed < 3 && IsContinuationByte(cursor.CharAt(pos + take)))
			{
				take--;
				backed++;
			}
			if (take <= 0)
				take = LONG_STRING_PIECE;
			if (pieces > 0)
				Append(",");
			Newline();
			VyshkaJson.QuoteTo(this, cursor.Slice(pos, take));
			pos += take;
			pieces++;
		}
		Close("]", pieces > 0);
		Close("}", true);
	}

	// IsContinuationByte says whether a one-byte string is a UTF-8
	// continuation byte (0x80 to 0xBF).
	static bool IsContinuationByte(string c)
	{
		int code = c.ToAscii();
		if (code < 0)
			code += 256;
		return code >= 128 && code < 192;
	}

	// MaxDepth is the deepest container nesting written so far, the root
	// container counting as one.
	int MaxDepth()
	{
		return m_MaxDepth;
	}

	// Append adds one piece, which must carry no newline of its own (Quote
	// escapes them; number and literal text has none).
	void Append(string piece)
	{
		int length = piece.Length();
		m_Buffer += piece;
		m_BufferLength += length;
		m_Length += length;
		m_LineLength += length;
		if (m_BufferLength >= CHUNK)
		{
			m_Chunks.Insert(m_Buffer);
			m_Buffer = "";
			m_BufferLength = 0;
		}
	}

	// Open starts an array or object: the bracket, and in lines mode one
	// level of depth for the elements.
	void Open(string bracket)
	{
		Append(bracket);
		m_Depth++;
		if (m_Depth > m_MaxDepth)
			m_MaxDepth = m_Depth;
	}

	// Close ends one: the depth comes back, and in lines mode the bracket
	// sits on a line of its own when anything was written inside.
	void Close(string bracket, bool hadElements)
	{
		m_Depth--;
		if (hadElements)
			Newline();
		Append(bracket);
	}

	// Newline ends the current line in lines mode and indents the next; a
	// compact writer ignores it.
	void Newline()
	{
		if (!m_Lines)
			return;
		if (m_LineLength > m_LongestLine)
			m_LongestLine = m_LineLength;
		string lead = "\n";
		for (int i = 0; i < m_Depth; i++)
			lead += "\t";
		Append(lead);
		m_LineLength = m_Depth;
	}

	int Length()
	{
		return m_Length;
	}

	// LongestLine is the length of the longest line written so far, the
	// current one included.
	int LongestLine()
	{
		if (m_LineLength > m_LongestLine)
			return m_LineLength;
		return m_LongestLine;
	}

	// Join returns everything written as one string.
	string Join()
	{
		array<string> pieces = new array<string>;
		for (int i = 0; i < m_Chunks.Count(); i++)
			pieces.Insert(m_Chunks.Get(i));
		if (m_BufferLength > 0)
			pieces.Insert(m_Buffer);
		return JoinPieces(pieces);
	}

	// JoinPieces concatenates pieces in two levels: groups of JOIN_GROUP
	// first, then the groups. With pieces of a few KiB, each append pays for
	// a group's worth, or for the growing result once per group, rather
	// than for the whole result once per piece.
	static string JoinPieces(array<string> pieces)
	{
		int count = pieces.Count();
		if (count == 0)
			return "";
		if (count == 1)
			return pieces.Get(0);
		array<string> groups = new array<string>;
		int at = 0;
		while (at < count)
		{
			string group = "";
			int stop = at + JOIN_GROUP;
			if (stop > count)
				stop = count;
			for (int i = at; i < stop; i++)
				group += pieces.Get(i);
			groups.Insert(group);
			at = stop;
		}
		string result = "";
		for (int g = 0; g < groups.Count(); g++)
			result += groups.Get(g);
		return result;
	}

	// WriteFile prints every chunk to an open file, in order.
	void WriteFile(FileHandle handle)
	{
		for (int i = 0; i < m_Chunks.Count(); i++)
			FPrint(handle, m_Chunks.Get(i));
		if (m_BufferLength > 0)
			FPrint(handle, m_Buffer);
	}
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

	// Depth is the container nesting of this value: 0 for a scalar, 1 for
	// an empty or flat container, one more per level inside.
	int Depth()
	{
		if (m_Kind != VyshkaJsonKind.ARRAY_VALUE && m_Kind != VyshkaJsonKind.OBJECT_VALUE)
			return 0;
		int deepest = 0;
		if (m_Kind == VyshkaJsonKind.ARRAY_VALUE)
		{
			for (int i = 0; i < m_Items.Count(); i++)
			{
				int itemDepth = m_Items.Get(i).Depth();
				if (itemDepth > deepest)
					deepest = itemDepth;
			}
		}
		else
		{
			for (int k = 0; k < m_Values.Count(); k++)
			{
				int valueDepth = m_Values.Get(k).Depth();
				if (valueDepth > deepest)
					deepest = valueDepth;
			}
		}
		return deepest + 1;
	}

	// Unchunk folds the chunk objects a file writer produced for long
	// strings (VyshkaJsonWriter.WriteChunked) back into string values and
	// restores the keys the writer escaped, throughout the tree, and
	// returns the value to use in place of the one given (the same object
	// unless it was itself a chunk object).
	static VyshkaJsonValue Unchunk(VyshkaJsonValue value)
	{
		if (!value)
			return null;
		if (value.m_Kind == VyshkaJsonKind.ARRAY_VALUE)
		{
			for (int i = 0; i < value.m_Items.Count(); i++)
				value.m_Items.Set(i, Unchunk(value.m_Items.Get(i)));
			return value;
		}
		if (value.m_Kind != VyshkaJsonKind.OBJECT_VALUE)
			return value;
		if (value.m_Keys.Count() == 1 && value.m_Keys.Get(0) == VyshkaJsonWriter.LONG_STRING_KEY)
		{
			VyshkaJsonValue pieces = value.m_Values.Get(0);
			if (pieces && pieces.m_Kind == VyshkaJsonKind.ARRAY_VALUE)
			{
				array<string> texts = new array<string>;
				bool allStrings = true;
				for (int p = 0; p < pieces.m_Items.Count(); p++)
				{
					VyshkaJsonValue piece = pieces.m_Items.Get(p);
					if (!piece || piece.m_Kind != VyshkaJsonKind.STRING_VALUE)
					{
						allStrings = false;
						break;
					}
					texts.Insert(piece.m_Text);
				}
				if (allStrings)
					return NewString(VyshkaJsonWriter.JoinPieces(texts));
			}
		}
		for (int k = 0; k < value.m_Values.Count(); k++)
		{
			string key = value.m_Keys.Get(k);
			string plain = VyshkaJsonWriter.UnescapeKey(key);
			if (plain != key)
				value.m_Keys.Set(k, plain);
			value.m_Values.Set(k, Unchunk(value.m_Values.Get(k)));
		}
		return value;
	}

	// ---- serialization ----

	// Serialize is the compact text: what goes on the wire.
	string Serialize()
	{
		VyshkaJsonWriter writer = new VyshkaJsonWriter(false);
		WriteTo(writer);
		return writer.Join();
	}

	// SerializeLines is the same document with every element and member on
	// a line of its own: what goes in a file (VyshkaFiles.WriteJson).
	string SerializeLines()
	{
		VyshkaJsonWriter writer = new VyshkaJsonWriter(true);
		WriteTo(writer);
		return writer.Join();
	}

	void WriteTo(VyshkaJsonWriter writer)
	{
		switch (m_Kind)
		{
			case VyshkaJsonKind.NULL_VALUE:
				writer.Append("null");
				break;
			case VyshkaJsonKind.BOOL_VALUE:
				if (m_Bool)
					writer.Append("true");
				else
					writer.Append("false");
				break;
			case VyshkaJsonKind.NUMBER_VALUE:
				writer.Append(m_Text);
				break;
			case VyshkaJsonKind.STRING_VALUE:
				if (writer.ChunksString(m_Text.Length()))
					writer.WriteChunked(m_Text);
				else
					VyshkaJson.QuoteTo(writer, m_Text);
				break;
			case VyshkaJsonKind.ARRAY_VALUE:
				writer.Open("[");
				int itemCount = m_Items.Count();
				for (int i = 0; i < itemCount; i++)
				{
					if (i > 0)
						writer.Append(",");
					writer.Newline();
					m_Items.Get(i).WriteTo(writer);
				}
				writer.Close("]", itemCount > 0);
				break;
			case VyshkaJsonKind.OBJECT_VALUE:
				writer.Open("{");
				int keyCount = m_Keys.Count();
				for (int k = 0; k < keyCount; k++)
				{
					if (k > 0)
						writer.Append(",");
					writer.Newline();
					string key = m_Keys.Get(k);
					if (writer.ChunksStrings())
						key = VyshkaJsonWriter.EscapeKey(key);
					VyshkaJson.QuoteTo(writer, key);
					writer.Append(":");
					// A long key takes its line alone, so that with a value
					// under the chunk threshold beside it the line still
					// stays under the reader's limit; whitespace after the
					// colon is JSON's own.
					if (writer.ChunksStrings() && key.Length() > VyshkaJsonWriter.LONG_KEY_ALONE)
						writer.Newline();
					m_Values.Get(k).WriteTo(writer);
				}
				writer.Close("}", keyCount > 0);
				break;
		}
	}
}

// VyshkaJson holds the parser and the string quoting helper. Parse returns
// null on malformed input and never throws: a bad response body is a
// transport failure to retry, not a reason to crash the game server.
class VyshkaJson
{
	protected ref VyshkaTextCursor m_Cursor;
	protected int m_Pos;
	protected int m_Length;
	protected int m_Depth;
	protected int m_MaxDepth;
	protected bool m_Failed;

	// How deep a document from the wire may nest. A file the plugin wrote
	// may nest FILE_MAX_DEPTH: whatever the wire allowed, plus the record
	// around it (an outbox record, a rejected record's two levels) and the
	// two levels a chunked string adds, so anything the plugin could parse
	// can be persisted and read back. Both sit well inside what the script
	// VM allows the parser's recursion: on DayZ 1.29 the parser read 56
	// nested arrays and the VM threw a stack overflow exception at 64
	// (measured by the self-test's json.depth check with these bounds
	// raised for the run, from a shallow call stack; the plugin's own
	// callers stand a few frames deeper).
	static const int MAX_DEPTH = 32;
	static const int FILE_MAX_DEPTH = 40;
	// How much decoded text ParseString gathers before setting it aside as
	// a piece; an append past this pays for the piece, not the value.
	static const int PIECE_LENGTH = 4096;

	static VyshkaJsonValue Parse(string input)
	{
		VyshkaTextCursor cursor = VyshkaTextCursor.OfString(input);
		return ParseCursor(cursor, MAX_DEPTH);
	}

	// ParseSegments parses the segments as one text, concatenated verbatim
	// (VyshkaTextCursor.OfSegments): the lines of a file, each with its
	// newline, as VyshkaFiles.ReadSegments returns them, to the file depth.
	static VyshkaJsonValue ParseSegments(array<string> segments)
	{
		VyshkaTextCursor cursor = VyshkaTextCursor.OfSegments(segments);
		return ParseCursor(cursor, FILE_MAX_DEPTH);
	}

	static VyshkaJsonValue ParseCursor(VyshkaTextCursor cursor, int maxDepth)
	{
		VyshkaJson parser = new VyshkaJson();
		parser.m_Cursor = cursor;
		parser.m_Pos = 0;
		parser.m_Length = cursor.Length();
		parser.m_Depth = 0;
		parser.m_MaxDepth = maxDepth;
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
		VyshkaJsonWriter writer = new VyshkaJsonWriter(false);
		QuoteTo(writer, text);
		return writer.Join();
	}

	// QuoteTo writes the quoted form of text to a writer. The text is read
	// through a cursor, so a long value costs what its length is, and the
	// runs between escapes are copied out one cursor piece at a time, so no
	// run is cut at what Substring can return.
	static void QuoteTo(VyshkaJsonWriter writer, string text)
	{
		writer.Append("\"");
		VyshkaTextCursor cursor = VyshkaTextCursor.OfString(text);
		int length = cursor.Length();
		int runStart = 0;
		for (int i = 0; i < length; i++)
		{
			string c = cursor.CharAt(i);
			string escaped = Escape(c);
			if (escaped != "")
			{
				if (i > runStart)
					writer.Append(cursor.Slice(runStart, i - runStart));
				writer.Append(escaped);
				runStart = i + 1;
			}
			else if (i + 1 == cursor.WindowEnd())
			{
				writer.Append(cursor.Slice(runStart, i + 1 - runStart));
				runStart = i + 1;
			}
		}
		if (length > runStart)
			writer.Append(cursor.Slice(runStart, length - runStart));
		writer.Append("\"");
	}

	// Escape is the escape sequence for one character, or "" when it needs
	// none. The engine's script parser cannot scan a literal that combines
	// a backslash escape with a second escape, so the backslash is built
	// from its byte value rather than written as "\\".
	static string Escape(string c)
	{
		if (c == "\"")
			return Backslash() + "\"";
		if (c == Backslash())
			return Backslash() + Backslash();
		int code = c.ToAscii();
		if (code < 0 || code >= 32)
			return "";
		if (code == 10)
			return Backslash() + "n";
		if (code == 13)
			return Backslash() + "r";
		if (code == 9)
			return Backslash() + "t";
		if (code == 8)
			return Backslash() + "b";
		if (code == 12)
			return Backslash() + "f";
		return Backslash() + "u00" + HexByte(code);
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
		return m_Cursor.CharAt(m_Pos);
	}

	protected void SkipWhitespace()
	{
		while (m_Pos < m_Length)
		{
			string c = m_Cursor.CharAt(m_Pos);
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
		if (m_Cursor.Slice(m_Pos, length) != literal)
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
		if (m_Depth >= m_MaxDepth)
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
		if (m_Depth >= m_MaxDepth)
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
	// The unescaped runs are copied out through the cursor, which assembles
	// a run of any length from pieces Substring can return whole; the first
	// parser copied each run with one Substring and so cut any run past
	// 8 191 characters without a word (issue #108). The decoded text is
	// gathered in pieces of a few KiB and joined once, so a long value
	// dense with escapes costs its length rather than its square.
	protected bool ParseString(out string text)
	{
		m_Pos++; // opening quote
		text = "";
		array<string> pieces = new array<string>;
		int textLength = 0;
		int runStart = m_Pos;
		while (m_Pos < m_Length)
		{
			string c = m_Cursor.CharAt(m_Pos);
			if (c == "\"")
			{
				if (m_Pos > runStart)
					text += m_Cursor.Slice(runStart, m_Pos - runStart);
				m_Pos++;
				if (pieces.Count() > 0)
				{
					pieces.Insert(text);
					text = VyshkaJsonWriter.JoinPieces(pieces);
				}
				return true;
			}
			if (textLength >= PIECE_LENGTH)
			{
				pieces.Insert(text);
				text = "";
				textLength = 0;
			}
			if (c == "\\")
			{
				if (m_Pos > runStart)
				{
					text += m_Cursor.Slice(runStart, m_Pos - runStart);
					textLength += m_Pos - runStart;
				}
				textLength++;
				m_Pos++;
				if (m_Pos >= m_Length)
					return false;
				string e = m_Cursor.CharAt(m_Pos);
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
						if (m_Pos + 1 < m_Length && m_Cursor.CharAt(m_Pos) == "\\" && m_Cursor.CharAt(m_Pos + 1) == "u")
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
			int digit = HexDigit(m_Cursor.CharAt(m_Pos + i));
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
		v.m_Text = m_Cursor.Slice(start, m_Pos - start);
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
