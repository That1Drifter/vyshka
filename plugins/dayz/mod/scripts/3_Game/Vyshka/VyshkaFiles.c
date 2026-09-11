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

class VyshkaFiles
{
	static const string ROOT = "$profile:Vyshka";
	static const string CONFIG_PATH = "$profile:Vyshka/config.json";
	static const string CREDENTIALS_PATH = "$profile:Vyshka/credentials.json";
	static const string OUTBOX_DIR = "$profile:Vyshka/outbox";
	static const string REJECTED_DIR = "$profile:Vyshka/rejected";
	static const string EXECUTED_PATH = "$profile:Vyshka/executed.log";

	// EnsureLayout creates the directories, one level at a time, because
	// MakeDirectory creates only the last path segment.
	static void EnsureLayout()
	{
		if (!FileExist(ROOT))
			MakeDirectory(ROOT);
		if (!FileExist(OUTBOX_DIR))
			MakeDirectory(OUTBOX_DIR);
		if (!FileExist(REJECTED_DIR))
			MakeDirectory(REJECTED_DIR);
	}

	// ReadAll returns the whole file as one string, lines joined with "\n".
	static bool ReadAll(string path, out string content)
	{
		content = "";
		if (!FileExist(path))
			return false;
		FileHandle handle = OpenFile(path, FileMode.READ);
		if (handle == 0)
			return false;
		string line;
		bool first = true;
		while (FGets(handle, line) >= 0)
		{
			if (!first)
				content += "\n";
			content += line;
			first = false;
		}
		CloseFile(handle);
		return true;
	}

	static bool WriteAll(string path, string content)
	{
		FileHandle handle = OpenFile(path, FileMode.WRITE);
		if (handle == 0)
			return false;
		FPrint(handle, content);
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
		string content;
		if (!ReadAll(path, content))
			return null;
		return VyshkaJson.Parse(content);
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

	static const int SNAPSHOT_INTERVAL_DEFAULT = 10;
	static const int SNAPSHOT_INTERVAL_MIN = 2;
	static const int SNAPSHOT_INTERVAL_MAX = 600;

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
		return VyshkaFiles.WriteAll(VyshkaFiles.CREDENTIALS_PATH, root.Serialize());
	}

	static void Delete()
	{
		if (FileExist(VyshkaFiles.CREDENTIALS_PATH))
			DeleteFile(VyshkaFiles.CREDENTIALS_PATH);
	}
}
