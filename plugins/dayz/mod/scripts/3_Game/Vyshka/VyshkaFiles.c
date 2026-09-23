// Vyshka DayZ plugin: files under the server profile directory.
//
// Everything the plugin persists lives under $profile:Vyshka/. The profile
// directory is the one location script may write to on a dedicated server
// (it is where DeleteFile works), and -profiles= lets an operator put it
// wherever they like.
//
// Layout:
//   $profile:Vyshka/config.json        operator-written: hub URL, enrollment token
//   $profile:Vyshka/credentials.json   plugin-written after enrollment (spec section 5.2)
//   $profile:Vyshka/outbox/<n>.json    one unacked envelope per file (section 9.3)
//   $profile:Vyshka/rejected/<n>.json  an envelope the hub refused as malformed, set
//                                      aside with the hub's reason for the operator
//                                      (section 2.3); never sent again
//   $profile:Vyshka/bans.json          the plugin's ban list (VyshkaBans); operator-editable
//   $profile:Vyshka/installation-bans.json
//                                      the hub's installation ban list as last applied
//                                      (VyshkaInstallationBans); overwritten on every apply
//   $profile:Vyshka/manifest.json      the manifest revision last published and the content
//                                      it went with (VyshkaManifestRecord)
//   $profile:Vyshka/executed.log       the executed actionIds, one per line (VyshkaPlugin)
//   $profile:Vyshka/<name>.next.<ext>  the staging copy of a file being replaced (below);
//                                      present only while a rewrite is under way
//
// A file the plugin rewrites in place (the credentials, both ban lists, the
// manifest record, the executed log at its boot compaction) is replaced
// through a staging copy, never opened over directly: FileMode.WRITE
// truncates the file before a byte of the new content is in it, the engine
// offers no rename, and a process that died partway would leave the file cut
// short (issue #121). Replace writes the whole new content to
// <name>.next.<ext>, then to the file itself, then deletes the staging copy,
// so a crash in either write leaves one whole copy on disk: the old file
// while the staging copy is written, the staging copy while the file is.
// The engine reports no error from a write (a full disk included), so each
// write is read back (WriteChecked) before the next step destroys anything.
// Whoever reads such a file reads it through ReadReplacedJson or
// ReadReplacedLines, which finish an interrupted replace first: a JSON
// staging copy that parses is the newest content there is and is written
// over the file (Replace writes only an object or an array, and no prefix of
// one parses), and one cut short is discarded. A line staging copy is whole
// when it ends with LINES_END, a line the file itself never carries; when
// the file lacks some of its records, the staged history is appended to the
// file marked RESTORED, which readers put back in its place in the order
// (InRecencyOrder), and the file is never truncated there (RecoverLines). A staging copy that cannot be read,
// or a whole one that cannot be finished, is kept, since it may be the only
// whole copy, and no replace of its file goes ahead. Every replace starts
// from a disk holding no staging copy, which is what lets the staging write
// truncate whatever was there.
//
// The engine reads files a line at a time (FGets), and a line of 65 536
// bytes or more faults the process inside the engine, with no way to catch
// it from script (measured in spikes/dayz-bans-pull-size on DayZ 1.29;
// issue #108). Every JSON file the plugin writes therefore goes through
// WriteJson, which puts each array element and object member on a line of
// its own, breaks a long string value into pieces on lines of their own,
// and refuses a document that would still carry a line over LINE_MAX (or
// nest deeper than the parser reads) rather than write what the next boot
// could not read; and every JSON file is read through ReadJson, which
// parses the lines as they come instead of joining them into one string
// first (an append per line onto a growing string would cost the square
// of the file) and folds the pieces back.

class VyshkaFiles
{
	static const string ROOT = "$profile:Vyshka";
	static const string CONFIG_PATH = "$profile:Vyshka/config.json";
	static const string CREDENTIALS_PATH = "$profile:Vyshka/credentials.json";
	static const string OUTBOX_DIR = "$profile:Vyshka/outbox";
	static const string REJECTED_DIR = "$profile:Vyshka/rejected";
	static const string EXECUTED_PATH = "$profile:Vyshka/executed.log";
	static const string BANS_PATH = "$profile:Vyshka/bans.json";
	static const string INSTALLATION_BANS_PATH = "$profile:Vyshka/installation-bans.json";
	static const string MANIFEST_PATH = "$profile:Vyshka/manifest.json";

	// The last line of a whole line-file staging copy (ReplaceLines). It is
	// not a JSON string, so no record of the executed log can be it.
	static const string LINES_END = "#vyshka:end";

	// The mark on a record a recovery appended to a line file
	// (RecoverLines); like LINES_END it cannot begin a JSON string.
	static const string RESTORED = "#vyshka:restored ";

	// The longest line a file written here may carry. The engine's reader
	// returned a 65 520-byte line intact and faulted on 65 536; the margin
	// below that covers the newline and whatever the reader counts that the
	// measurement could not see.
	static const int LINE_MAX = 60000;

	// Lines are gathered in groups while a file is read, so no append pays
	// for more than a group.
	static const int READ_GROUP_BYTES = 8192;

	// EnsureLayout creates the directories, one level at a time, because
	// MakeDirectory creates only the last path segment. The files directly
	// under the root (the credentials, the ban list, the manifest revision)
	// need no directory of their own; the root is made here for them.
	static void EnsureLayout()
	{
		if (!FileExist(ROOT))
			MakeDirectory(ROOT);
		if (!FileExist(OUTBOX_DIR))
			MakeDirectory(OUTBOX_DIR);
		if (!FileExist(REJECTED_DIR))
			MakeDirectory(REJECTED_DIR);
	}

	// ReadSegments returns the file's lines, each with a newline appended,
	// so concatenated they are the file's text with its line breaks; false
	// when the file is missing or cannot be opened. A JSON reader parses
	// them in place (VyshkaJson.ParseSegments) rather than joining them.
	static bool ReadSegments(string path, out array<string> segments)
	{
		segments = new array<string>;
		if (!FileExist(path))
			return false;
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return false;
		string line;
		while (FGets(handle, line) >= 0)
			segments.Insert(line + "\n");
		CloseFile(handle);
		return true;
	}

	// ReadAll returns the whole file as one string, lines joined with "\n"
	// and no newline after the last. The lines are joined in groups, so a
	// file of many short lines costs its length rather than its square.
	static bool ReadAll(string path, out string content)
	{
		content = "";
		if (!FileExist(path))
			return false;
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return false;
		array<string> groups = new array<string>;
		string group = "";
		int groupLength = 0;
		string line;
		bool first = true;
		while (FGets(handle, line) >= 0)
		{
			if (!first)
			{
				group += "\n";
				groupLength++;
			}
			first = false;
			group += line;
			groupLength += line.Length();
			if (groupLength >= READ_GROUP_BYTES)
			{
				groups.Insert(group);
				group = "";
				groupLength = 0;
			}
		}
		CloseFile(handle);
		if (groupLength > 0)
			groups.Insert(group);
		content = VyshkaJsonWriter.JoinPieces(groups);
		return true;
	}

	// WriteAll writes one string as the file's whole content. The caller
	// keeps every line of it under LINE_MAX, or the file cannot be read
	// back; a JSON document goes through WriteJson, which sees to that.
	static bool WriteAll(string path, string content)
	{
		return WriteOut(path, null, content);
	}

	// WriteJson writes a JSON document straight over a file, which a crash
	// partway leaves cut short: it is for a file written once (an outbox
	// record), and a file rewritten in place goes through ReplaceJson. It
	// puts every array element and object
	// member on a line of its own (VyshkaJsonValue.SerializeLines), chunk
	// by chunk, never as one string, and with every long string value
	// broken into pieces on lines of their own (VyshkaJsonWriter.WriteChunked;
	// ReadJson folds them back). It refuses, with an error in the log and
	// the file left as it was, a document any line of which would still
	// pass LINE_MAX (only a key or a number token of that length can), or
	// one nested deeper than the parser reads, since a file that faults or
	// fails the next boot is worse than a record not kept.
	static bool WriteJson(string path, VyshkaJsonValue value)
	{
		VyshkaJsonWriter writer = PrepareJson(path, value);
		if (!writer)
			return false;
		return WriteOut(path, writer, "");
	}

	// PrepareJson serializes a document for a file, or refuses it as
	// WriteJson describes and returns null.
	static VyshkaJsonWriter PrepareJson(string path, VyshkaJsonValue value)
	{
		VyshkaJsonWriter writer = new VyshkaJsonWriter(true);
		writer.ChunkStrings(true);
		value.WriteTo(writer);
		int longest = writer.LongestLine();
		if (longest > LINE_MAX)
		{
			VyshkaLog.Error("not writing " + path + ": a line of it would be " + longest.ToString() + " bytes, and the engine's file reader faults the server on a line over " + LINE_MAX.ToString());
			return null;
		}
		int depth = writer.MaxDepth();
		if (depth > VyshkaJson.FILE_MAX_DEPTH)
		{
			VyshkaLog.Error("not writing " + path + ": it nests " + depth.ToString() + " levels deep and the plugin reads a file no deeper than " + VyshkaJson.FILE_MAX_DEPTH.ToString());
			return null;
		}
		return writer;
	}

	// WriteOut opens a file over and writes either a prepared document or,
	// with no writer, the text. false only when the file could not be
	// opened, and then it was not touched. The engine reports no error from
	// a write itself (a full disk included); WriteChecked reads it back.
	static bool WriteOut(string path, VyshkaJsonWriter writer, string text)
	{
		FileHandle handle = OpenFile(path, FileMode.WRITE);
		if (handle == 0)
			return false;
		if (writer)
			writer.WriteFile(handle);
		else
			FPrint(handle, text);
		CloseFile(handle);
		return true;
	}

	// What WriteChecked found.
	static const int WRITTEN = 0;
	static const int NOT_OPENED = 1;   // the file was not touched
	static const int SHORT = 2;        // the file was truncated and holds less than was written

	// WriteChecked is WriteOut followed by a read-back of the file's length,
	// each line and its newline, against what was written: a document ends
	// with its closing bracket and no newline, so it reads back one byte
	// longer than it is; text a replace writes ends with a newline or is
	// empty, and reads back as long as it is.
	static int WriteChecked(string path, VyshkaJsonWriter writer, string text)
	{
		if (!WriteOut(path, writer, text))
			return NOT_OPENED;
		int expected = text.Length();
		if (writer)
			expected = writer.Length() + 1;
		if (ReadBackLength(path) != expected)
			return SHORT;
		return WRITTEN;
	}

	// ReadBackLength is a file's length as its lines read back, each with
	// its newline; -1 when it cannot be opened.
	static int ReadBackLength(string path)
	{
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return -1;
		int total = 0;
		string line;
		while (FGets(handle, line) >= 0)
			total += line.Length() + 1;
		CloseFile(handle);
		return total;
	}

	// Readable says whether a file that exists can be opened for reading.
	static bool Readable(string path)
	{
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return false;
		CloseFile(handle);
		return true;
	}

	// Discard removes a staging copy that must not be read as whole again.
	// One that cannot be deleted is emptied instead, which reads as cut
	// short; false when neither worked.
	static bool Discard(string staged)
	{
		if (!FileExist(staged) || DeleteFile(staged))
			return true;
		if (WriteOut(staged, null, ""))
			return true;
		VyshkaLog.Error("could neither delete nor empty " + staged);
		return false;
	}

	// StagingPath is where a file is staged while it is replaced: its name
	// with ".next" before the extension (bans.json, bans.next.json).
	static string StagingPath(string path)
	{
		int slash = path.LastIndexOf("/");
		int dot = path.LastIndexOf(".");
		if (dot <= slash)
			return path + ".next";
		return path.Substring(0, dot) + ".next" + path.Substring(dot, path.Length() - dot);
	}

	// ReplaceJson replaces a file with a JSON object or array through its
	// staging copy (see the top of this file). true once the file holds the
	// new document; false, with the file as it was, when the document is
	// refused (WriteJson), when either copy cannot be opened, or when a
	// staging copy left by an earlier replace could not be finished.
	static bool ReplaceJson(string path, VyshkaJsonValue value)
	{
		if (!value || (!value.IsObject() && !value.IsArray()))
		{
			VyshkaLog.Error("not writing " + path + ": only an object or an array can be replaced safely");
			return false;
		}
		VyshkaJsonValue unused;
		if (!RecoverJson(path, unused))
			return false;
		VyshkaJsonWriter writer = PrepareJson(path, value);
		if (!writer)
			return false;
		return Replace(path, writer, "", "");
	}

	// ReplaceLines replaces a file with lines, each of which carries no
	// newline and none of which is LINES_END, through its staging copy; the
	// staging copy alone ends with LINES_END, which says it is whole. The
	// outcome is ReplaceJson's.
	static bool ReplaceLines(string path, array<string> lines)
	{
		array<string> recovered;
		if (!RecoverLines(path, recovered))
			return false;
		array<string> pieces = new array<string>;
		for (int i = 0; i < lines.Count(); i++)
			pieces.Insert(lines.Get(i) + "\n");
		string body = VyshkaJsonWriter.JoinPieces(pieces);
		return Replace(path, null, body, body + LINES_END + "\n");
	}

	// Replace writes the staging copy, then the file, then deletes the
	// staging copy, reading each write back before the next step destroys
	// anything. A staging copy that could not be written whole is discarded
	// and the file was never touched. A file that could not be opened was
	// not touched either, and the staging copy is discarded with it, so the
	// disk holds what it did before. Two cases change the disk on a false
	// return, each with an error saying so: a file written short (a full
	// disk), where the staging copy is the only whole copy, is kept, and is
	// what the next read takes; and a file that could not be opened beside a
	// staging copy that can be neither deleted nor emptied.
	static bool Replace(string path, VyshkaJsonWriter writer, string text, string stagedText)
	{
		string staged = StagingPath(path);
		if (WriteChecked(staged, writer, stagedText) != WRITTEN)
		{
			Discard(staged);
			VyshkaLog.Error("could not write " + staged + " whole; " + path + " is left as it was");
			return false;
		}
		int result = WriteChecked(path, writer, text);
		if (result == NOT_OPENED)
		{
			if (Discard(staged))
				VyshkaLog.Error("could not write " + path + "; it is left as it was");
			else
				VyshkaLog.Error("could not write " + path + ", and " + staged + " could neither be deleted nor emptied: the next read of " + path + " takes the new content from it, though this write failed. Delete it before a restart to keep " + path + " as it was");
			return false;
		}
		if (result == SHORT)
		{
			VyshkaLog.Error(path + " was written short (is the disk full?); " + staged + " holds the new content whole and is kept, and the next read of " + path + " finishes the write");
			return false;
		}
		DeleteFile(staged);
		return true;
	}

	// RecoverJson finishes or discards a replace of a JSON file that a crash
	// interrupted. A staging copy that parses is whole and newest: staged is
	// set to it, and it is written over the file. One that does not parse was
	// cut short and is discarded, and the file holds what it was replacing.
	// false when a staging copy is left that may be the only whole copy: one
	// that cannot be read, or a whole one that could not be written over the
	// file (staged is then set, and what a reader reads).
	static bool RecoverJson(string path, out VyshkaJsonValue staged)
	{
		staged = null;
		string stagedPath = StagingPath(path);
		if (!FileExist(stagedPath))
			return true;
		array<string> segments;
		if (!ReadSegments(stagedPath, segments))
		{
			VyshkaLog.Error(stagedPath + " cannot be read, and may be the only whole copy of " + path + "; it is kept, and nothing is written to " + path + " until it can be read");
			return false;
		}
		staged = ParseRead(segments);
		if (!staged || (!staged.IsObject() && !staged.IsArray()))
		{
			staged = null;
			VyshkaLog.Warn(stagedPath + " was cut short while being written and is discarded; " + path + " still holds what it was replacing");
			Discard(stagedPath);
			return true;
		}
		VyshkaLog.Warn("finishing an interrupted write of " + path + " from " + stagedPath);
		VyshkaJsonWriter writer = PrepareJson(path, staged);
		if (!writer || WriteChecked(path, writer, "") != WRITTEN)
		{
			VyshkaLog.Error("could not finish the interrupted write of " + path + "; " + stagedPath + " is kept and read in its place, and nothing is written to " + path + " until it can be");
			return false;
		}
		DeleteFile(stagedPath);
		return true;
	}

	// RecoverLines is RecoverJson for a line file that is a log of records
	// whose order matters only as recency (the executed log). A staging copy
	// is whole when its last line is LINES_END. The file is never truncated
	// here, since it may hold records appended after the replace, and those
	// are the newest there are.
	//
	// A file that holds every staged record (a crash before the file was
	// opened, or a staging copy that outlived its replace) is complete and in
	// order as it stands, and the staging copy is deleted. One that lacks
	// some was truncated by the replace: it holds a prefix of the staged
	// records and then whatever was appended since, all of it newer than the
	// staged history. The whole staged history is then appended to it, each
	// record marked RESTORED (after a newline that ends any partial last
	// line), which says where it belongs in the order however many boots
	// read the file before it is next compacted: InRecencyOrder puts the
	// restored records first and drops the file's own copies of them. The
	// staging copy is deleted only once the file reads back holding every
	// staged record.
	//
	// staged is set to the records oldest first. false when a staging copy
	// is left that may be the only whole copy of some records: one that
	// cannot be read (staged null), or a whole one the file could not be
	// read or completed from (staged is then what a reader reads).
	static bool RecoverLines(string path, out array<string> staged)
	{
		staged = null;
		string stagedPath = StagingPath(path);
		if (!FileExist(stagedPath))
			return true;
		array<string> lines;
		if (!TryReadLines(stagedPath, lines))
		{
			VyshkaLog.Error(stagedPath + " cannot be read, and may hold the only copy of records of " + path + "; it is kept, and nothing is written to " + path + " until it can be read");
			return false;
		}
		int count = lines.Count();
		if (count == 0 || lines.Get(count - 1) != LINES_END)
		{
			VyshkaLog.Warn(stagedPath + " was cut short while being written and is discarded; " + path + " still holds what it was replacing");
			Discard(stagedPath);
			return true;
		}
		lines.Remove(count - 1);
		array<string> raw;
		if (!TryReadLines(path, raw))
		{
			staged = lines;
			VyshkaLog.Error(path + " cannot be read; " + stagedPath + " is kept and read in its place, and nothing is written to " + path + " until it can be");
			return false;
		}
		array<string> current = InRecencyOrder(raw);
		int missing = CountMissing(lines, current);
		VyshkaLog.Warn("finishing an interrupted write of " + path + " from " + stagedPath + ": " + missing.ToString() + " record(s) to restore");
		if (missing == 0)
		{
			staged = current;
			DeleteFile(stagedPath);
			return true;
		}
		// What the file will read as once the restored block is on it.
		staged = new array<string>;
		map<string, bool> inStaged = new map<string, bool>;
		for (int i = 0; i < lines.Count(); i++)
		{
			staged.Insert(lines.Get(i));
			inStaged.Set(lines.Get(i), true);
		}
		for (int j = 0; j < current.Count(); j++)
		{
			if (!inStaged.Contains(current.Get(j)))
				staged.Insert(current.Get(j));
		}
		array<string> pieces = new array<string>;
		pieces.Insert("\n");
		for (int k = 0; k < lines.Count(); k++)
			pieces.Insert(RESTORED + lines.Get(k) + "\n");
		AppendText(path, VyshkaJsonWriter.JoinPieces(pieces));
		array<string> after;
		if (!TryReadLines(path, after) || CountMissing(lines, InRecencyOrder(after)) > 0)
		{
			VyshkaLog.Error("could not restore the records " + path + " lacks; " + stagedPath + " is kept and read with it, and nothing is written to " + path + " until it can be");
			return false;
		}
		DeleteFile(stagedPath);
		return true;
	}

	// CountMissing is how many of the records are not among the lines.
	static int CountMissing(array<string> records, array<string> lines)
	{
		map<string, bool> held = new map<string, bool>;
		for (int i = 0; i < lines.Count(); i++)
			held.Set(lines.Get(i), true);
		int missing = 0;
		for (int j = 0; j < records.Count(); j++)
		{
			if (!held.Contains(records.Get(j)))
				missing++;
		}
		return missing;
	}

	// InRecencyOrder returns a log's lines oldest first: the records a
	// recovery restored (lines marked RESTORED, the mark removed, each once)
	// ahead of every other line, which keeps its place unless it is the
	// file's own copy of a restored record. A log with nothing restored is
	// in order as it stands.
	static array<string> InRecencyOrder(array<string> raw)
	{
		array<string> restored = new array<string>;
		map<string, bool> isRestored = new map<string, bool>;
		int markLength = RESTORED.Length();
		for (int i = 0; i < raw.Count(); i++)
		{
			string line = raw.Get(i);
			if (line.Length() <= markLength || line.Substring(0, markLength) != RESTORED)
				continue;
			// A record can pass the 8 191 characters one Substring returns.
			string record = VyshkaTextCursor.OfString(line).Slice(markLength, line.Length() - markLength);
			if (isRestored.Contains(record))
				continue;
			isRestored.Set(record, true);
			restored.Insert(record);
		}
		if (restored.Count() == 0)
			return raw;
		for (int j = 0; j < raw.Count(); j++)
		{
			string other = raw.Get(j);
			if (other.Length() > markLength && other.Substring(0, markLength) == RESTORED)
				continue;
			if (!isRestored.Contains(other))
				restored.Insert(other);
		}
		return restored;
	}

	// AppendText adds text to the end of a file, creating it when absent;
	// false when it cannot be opened.
	static bool AppendText(string path, string text)
	{
		FileHandle handle = OpenFile(path, FileMode.APPEND);
		if (handle == 0)
			return false;
		FPrint(handle, text);
		CloseFile(handle);
		return true;
	}

	// ReadReplacedJson reads a file written through ReplaceJson, finishing
	// or discarding an interrupted replace first; null when there is no
	// usable document. A whole staging copy that could not be written over
	// the file is what is read.
	static VyshkaJsonValue ReadReplacedJson(string path)
	{
		VyshkaJsonValue staged;
		RecoverJson(path, staged);
		if (staged)
			return staged;
		return ReadJson(path);
	}

	// ReadReplacedLines is ReadReplacedJson for a file written through
	// ReplaceLines: its records oldest first (InRecencyOrder), empty lines
	// dropped.
	static array<string> ReadReplacedLines(string path)
	{
		array<string> lines;
		TryReadReplacedLines(path, lines);
		return lines;
	}

	// TryReadReplacedLines is ReadReplacedLines that says whether what it
	// read is the whole history: false when a staging copy or the file could
	// not be read, or a recovery could not be finished, and then nothing may
	// be written over the file on the strength of what was read.
	static bool TryReadReplacedLines(string path, out array<string> lines)
	{
		array<string> staged;
		bool recovered = RecoverLines(path, staged);
		if (staged)
		{
			lines = staged;
			return recovered;
		}
		array<string> raw;
		bool read = TryReadLines(path, raw);
		lines = InRecencyOrder(raw);
		return recovered && read;
	}

	// DeleteReplaced deletes a file written through a replace, with any
	// staging copy of it, so no interrupted replace can bring it back.
	static void DeleteReplaced(string path)
	{
		string staged = StagingPath(path);
		if (FileExist(staged))
			DeleteFile(staged);
		if (FileExist(path))
			DeleteFile(path);
	}

	// AppendLine adds one line to a file, creating it when absent. It
	// starts with a newline of its own, so a last line left without one (a
	// crash inside an earlier append or rewrite) ends there instead of
	// running into this one; the empty line that leaves otherwise is
	// dropped by every reader here. The close is the only flush the engine
	// exposes.
	static bool AppendLine(string path, string line)
	{
		FileHandle handle = OpenFile(path, FileMode.APPEND);
		if (handle == 0)
			return false;
		FPrint(handle, "\n");
		FPrintln(handle, line);
		CloseFile(handle);
		return true;
	}

	// ReadLines returns a file's lines, empty ones dropped. Missing file is an
	// empty list, not an error.
	static array<string> ReadLines(string path)
	{
		array<string> lines;
		TryReadLines(path, lines);
		return lines;
	}

	// TryReadLines is ReadLines that says whether the file could be read:
	// false when it exists and cannot be opened (lines is then empty). A
	// missing file reads as no lines.
	static bool TryReadLines(string path, out array<string> lines)
	{
		lines = new array<string>;
		if (!FileExist(path))
			return true;
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return false;
		string line;
		while (FGets(handle, line) >= 0)
		{
			if (line != "")
				lines.Insert(line);
		}
		CloseFile(handle);
		return true;
	}

	// ReadJson parses a file's content; null when missing or malformed.
	static VyshkaJsonValue ReadJson(string path)
	{
		array<string> segments;
		if (!ReadSegments(path, segments))
			return null;
		return ParseRead(segments);
	}

	// ParseRead parses the lines ReadSegments read.
	static VyshkaJsonValue ParseRead(array<string> segments)
	{
		VyshkaJsonValue root = VyshkaJson.ParseSegments(segments);
		// Only a file this writer produced carries chunked strings and
		// escaped keys, and such a file with anything in it spans lines;
		// a file plugin 0.8.0 wrote is one line, and reads as it is, so a
		// key or an object of its own that happens to look like the
		// writer's marks is left alone.
		if (segments.Count() > 1)
			return VyshkaJsonValue.Unchunk(root);
		return root;
	}
}

// VyshkaConfig is what the operator writes: where the hub is and the one-time
// enrollment token from the operator's server record (spec section 5).
class VyshkaConfig
{
	string m_HubUrl;
	string m_EnrollmentToken;
	int m_PollTimeoutSeconds;
	string m_Game;
	// How often a state.players snapshot is published (spec section 8.3);
	// 0 turns snapshots off. Bounded below so a typo cannot make the plugin
	// sample every tick, and above so a stale map is still a map.
	int m_SnapshotIntervalSeconds;
	// How often a core.server.fps sample is published (spec section 8.1);
	// 0 turns the samples off. Bounded like the snapshot interval.
	int m_FpsIntervalSeconds;

	static const int SNAPSHOT_INTERVAL_DEFAULT = 10;
	static const int SNAPSHOT_INTERVAL_MIN = 2;
	static const int SNAPSHOT_INTERVAL_MAX = 600;
	static const int FPS_INTERVAL_DEFAULT = 60;
	static const int FPS_INTERVAL_MIN = 5;
	static const int FPS_INTERVAL_MAX = 3600;

	static VyshkaConfig Load()
	{
		VyshkaJsonValue root = VyshkaFiles.ReadJson(VyshkaFiles.CONFIG_PATH);
		if (!root || !root.IsObject())
			return null;
		VyshkaConfig config = new VyshkaConfig();
		config.m_HubUrl = root.GetString("hubUrl", "");
		config.m_EnrollmentToken = root.GetString("enrollmentToken", "");
		config.m_PollTimeoutSeconds = root.GetInt("pollTimeoutSeconds", 25);
		config.m_Game = root.GetString("game", "dayz");
		config.m_SnapshotIntervalSeconds = root.GetInt("snapshotIntervalSeconds", SNAPSHOT_INTERVAL_DEFAULT);
		if (config.m_SnapshotIntervalSeconds < 0)
			config.m_SnapshotIntervalSeconds = 0;
		if (config.m_SnapshotIntervalSeconds > 0 && config.m_SnapshotIntervalSeconds < SNAPSHOT_INTERVAL_MIN)
			config.m_SnapshotIntervalSeconds = SNAPSHOT_INTERVAL_MIN;
		if (config.m_SnapshotIntervalSeconds > SNAPSHOT_INTERVAL_MAX)
			config.m_SnapshotIntervalSeconds = SNAPSHOT_INTERVAL_MAX;
		config.m_FpsIntervalSeconds = root.GetInt("fpsIntervalSeconds", FPS_INTERVAL_DEFAULT);
		if (config.m_FpsIntervalSeconds < 0)
			config.m_FpsIntervalSeconds = 0;
		if (config.m_FpsIntervalSeconds > 0 && config.m_FpsIntervalSeconds < FPS_INTERVAL_MIN)
			config.m_FpsIntervalSeconds = FPS_INTERVAL_MIN;
		if (config.m_FpsIntervalSeconds > FPS_INTERVAL_MAX)
			config.m_FpsIntervalSeconds = FPS_INTERVAL_MAX;
		if (config.m_HubUrl == "")
			return null;
		// The plugin appends the Plugin API path itself, so accept the hub's
		// base URL with or without a trailing slash.
		int length = config.m_HubUrl.Length();
		if (config.m_HubUrl.Substring(length - 1, 1) == "/")
			config.m_HubUrl = config.m_HubUrl.Substring(0, length - 1);
		if (config.m_PollTimeoutSeconds < 5)
			config.m_PollTimeoutSeconds = 5;
		if (config.m_PollTimeoutSeconds > 60)
			config.m_PollTimeoutSeconds = 60;
		return config;
	}
}

// VyshkaCredentials are the permanent server credentials enrollment issued
// (spec section 5.2). The enrollment token that produced them is kept as well,
// so a fresh token in the config file (the operator's recovery path after a
// revocation) is recognized as a request to enroll again.
class VyshkaCredentials
{
	string m_ServerId;
	string m_ServerSecret;
	string m_EnrolledWithToken;

	static VyshkaCredentials Load()
	{
		VyshkaJsonValue root = VyshkaFiles.ReadReplacedJson(VyshkaFiles.CREDENTIALS_PATH);
		if (!root || !root.IsObject())
			return null;
		VyshkaCredentials credentials = new VyshkaCredentials();
		credentials.m_ServerId = root.GetString("serverId", "");
		credentials.m_ServerSecret = root.GetString("serverSecret", "");
		credentials.m_EnrolledWithToken = root.GetString("enrolledWithToken", "");
		if (credentials.m_ServerId == "" || credentials.m_ServerSecret == "")
			return null;
		return credentials;
	}

	bool Save()
	{
		VyshkaJsonValue root = VyshkaJsonValue.NewObject();
		root.Set("serverId", VyshkaJsonValue.NewString(m_ServerId));
		root.Set("serverSecret", VyshkaJsonValue.NewString(m_ServerSecret));
		root.Set("enrolledWithToken", VyshkaJsonValue.NewString(m_EnrolledWithToken));
		return VyshkaFiles.ReplaceJson(VyshkaFiles.CREDENTIALS_PATH, root);
	}

	static void Delete()
	{
		VyshkaFiles.DeleteReplaced(VyshkaFiles.CREDENTIALS_PATH);
	}
}

// VyshkaManifestRecord is $profile:Vyshka/manifest.json: the revision the
// plugin last published and the manifest content it went with, so the next
// boot can tell whether it changed anything (VyshkaPlugin.ResolveManifestRevision),
// with the marks that say whether a hub was seen to accept the revision.
//
// The content is kept as the manifest object itself, so its arrays break
// across lines like everything else the plugin writes. Plugin 0.8.0 kept
// it as one JSON string, which no line writer can break (a newline inside
// a JSON string is escaped), so a manifest of a few hundred actions ran
// past the reader's limit as a single token (issue #108); such a record is
// still read, and the next save replaces it.
class VyshkaManifestRecord
{
	int m_Revision;
	bool m_Pending;     // minted here and no hub yet seen to accept it
	int m_Above;        // the hub revision the pending one was published above, or -2 for no mark
	string m_Content;   // the content as it serializes compact, which is what a boot compares

	// Load reads the record; null, with problem saying why, when there is
	// none that can be used.
	static VyshkaManifestRecord Load(out string problem)
	{
		problem = "";
		VyshkaJsonValue root = VyshkaFiles.ReadReplacedJson(VyshkaFiles.MANIFEST_PATH);
		if (!root && !FileExist(VyshkaFiles.MANIFEST_PATH))
		{
			problem = "no " + VyshkaFiles.MANIFEST_PATH + " yet";
			return null;
		}
		if (!root || !root.IsObject())
		{
			problem = VyshkaFiles.MANIFEST_PATH + " did not parse as a JSON object";
			return null;
		}
		VyshkaManifestRecord record = new VyshkaManifestRecord();
		record.m_Revision = root.GetInt("revision", 0);
		// A record without the mark (written before it existed) has no
		// evidence of acceptance either, so it reads as pending.
		record.m_Pending = root.GetBool("pending", true);
		// Only a hub revision (section 6: 1 or more) is evidence of where
		// the hub stood; anything else in the mark reads as no mark.
		record.m_Above = root.GetInt("above", -2);
		if (record.m_Above < 1)
			record.m_Above = -2;
		VyshkaJsonValue content = root.Get("content");
		if (content && content.IsObject())
			record.m_Content = content.Serialize();
		else if (content && content.IsString())
			record.m_Content = content.m_Text;
		else
		{
			problem = VyshkaFiles.MANIFEST_PATH + " carries no content";
			return null;
		}
		return record;
	}

	// Save writes the record; false when it could not be written.
	static bool Save(int revision, VyshkaJsonValue content, bool pending, int above)
	{
		VyshkaJsonValue record = VyshkaJsonValue.NewObject();
		record.Set("revision", VyshkaJsonValue.NewInt(revision));
		record.Set("pending", VyshkaJsonValue.NewBool(pending));
		if (pending && above != -2)
			record.Set("above", VyshkaJsonValue.NewInt(above));
		record.Set("content", content);
		return VyshkaFiles.ReplaceJson(VyshkaFiles.MANIFEST_PATH, record);
	}
}
