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
//                                      (VyshkaInstallationBans); overwritten on every apply,
//                                      through installation-bans.next.json so a crash
//                                      mid-write never loses the list in force
//   $profile:Vyshka/manifest.json      the manifest revision last published and the content
//                                      it went with (VyshkaManifestRecord)
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
	static const string INSTALLATION_BANS_NEXT_PATH = "$profile:Vyshka/installation-bans.next.json";
	static const string MANIFEST_PATH = "$profile:Vyshka/manifest.json";

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
		FileHandle handle = OpenFile(path, FileMode.WRITE);
		if (handle == 0)
			return false;
		FPrint(handle, content);
		CloseFile(handle);
		return true;
	}

	// WriteJson writes a JSON document with every array element and object
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
		VyshkaJsonWriter writer = new VyshkaJsonWriter(true);
		writer.ChunkStrings(true);
		value.WriteTo(writer);
		int longest = writer.LongestLine();
		if (longest > LINE_MAX)
		{
			VyshkaLog.Error("not writing " + path + ": a line of it would be " + longest.ToString() + " bytes, and the engine's file reader faults the server on a line over " + LINE_MAX.ToString());
			return false;
		}
		int depth = writer.MaxDepth();
		if (depth > VyshkaJson.FILE_MAX_DEPTH)
		{
			VyshkaLog.Error("not writing " + path + ": it nests " + depth.ToString() + " levels deep and the plugin reads a file no deeper than " + VyshkaJson.FILE_MAX_DEPTH.ToString());
			return false;
		}
		FileHandle handle = OpenFile(path, FileMode.WRITE);
		if (handle == 0)
			return false;
		writer.WriteFile(handle);
		CloseFile(handle);
		return true;
	}

	// AppendLine adds one line to a file, creating it when absent. The close
	// is the only flush the engine exposes.
	static bool AppendLine(string path, string line)
	{
		FileHandle handle = OpenFile(path, FileMode.APPEND);
		if (handle == 0)
			return false;
		FPrintln(handle, line);
		CloseFile(handle);
		return true;
	}

	// ReadLines returns a file's lines, empty ones dropped. Missing file is an
	// empty list, not an error.
	static array<string> ReadLines(string path)
	{
		array<string> lines = new array<string>;
		if (!FileExist(path))
			return lines;
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return lines;
		string line;
		while (FGets(handle, line) >= 0)
		{
			if (line != "")
				lines.Insert(line);
		}
		CloseFile(handle);
		return lines;
	}

	// ReadJson parses a file's content; null when missing or malformed.
	static VyshkaJsonValue ReadJson(string path)
	{
		array<string> segments;
		if (!ReadSegments(path, segments))
			return null;
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
		VyshkaJsonValue root = VyshkaFiles.ReadJson(VyshkaFiles.CREDENTIALS_PATH);
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
		return VyshkaFiles.WriteJson(VyshkaFiles.CREDENTIALS_PATH, root);
	}

	static void Delete()
	{
		if (FileExist(VyshkaFiles.CREDENTIALS_PATH))
			DeleteFile(VyshkaFiles.CREDENTIALS_PATH);
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
		if (!FileExist(VyshkaFiles.MANIFEST_PATH))
		{
			problem = "no " + VyshkaFiles.MANIFEST_PATH + " yet";
			return null;
		}
		VyshkaJsonValue root = VyshkaFiles.ReadJson(VyshkaFiles.MANIFEST_PATH);
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
		return VyshkaFiles.WriteJson(VyshkaFiles.MANIFEST_PATH, record);
	}
}
