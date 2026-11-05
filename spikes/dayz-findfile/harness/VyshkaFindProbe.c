
// VyshkaFindProbe measures what FindFile and FindNextFile hand back for a
// directory under $profile: on this build: whether a handle comes back at
// all, and the exact names, for each pattern and flag set the plugin's
// outbox restore could use. The directory and its files are made fresh
// first, so every listing has the same files to find.
class VyshkaFindProbe
{
	static const string TAG = "VYSHKA_FIND";
	static const string DIR = "$profile:VyshkaFind";

	static void Run()
	{
		GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Measure, 2000, false);
	}

	static void Measure()
	{
		MakeDirectory(DIR);
		Write("1.json", "{\"small\": true}");
		Write("22.json", "{\"other\": true}");
		Write("note.txt", "not a record");
		Print(TAG + "\tfiles\tsmall=" + FileExist(DIR + "/1.json") + "\tother=" + FileExist(DIR + "/22.json") + "\ttext=" + FileExist(DIR + "/note.txt"));

		List("slash-json-dirs", DIR + "/*.json", FindFileFlags.DIRECTORIES);
		List("slash-json-all", DIR + "/*.json", FindFileFlags.ALL);
		List("slash-json-archives", DIR + "/*.json", FindFileFlags.ARCHIVES);
		List("slash-star-dirs", DIR + "/*", FindFileFlags.DIRECTORIES);
		List("slash-star-all", DIR + "/*", FindFileFlags.ALL);
		List("backslash-json-dirs", DIR + "\\*.json", FindFileFlags.DIRECTORIES);
		List("exact-dirs", DIR + "/1.json", FindFileFlags.DIRECTORIES);
		List("dir-itself-dirs", DIR, FindFileFlags.DIRECTORIES);
		List("profile-star-dirs", "$profile:*", FindFileFlags.DIRECTORIES);
		List("profile-slash-star-dirs", "$profile:/*", FindFileFlags.DIRECTORIES);
		List("saves-star-dirs", "$saves:*", FindFileFlags.DIRECTORIES);

		// The same directory named without the alias: through the -profiles
		// value the server was started with (relative to its working
		// directory, or absolute), and relative to the working directory.
		string profiles;
		bool given = GetGame().CommandlineGetParam("profiles", profiles);
		Print(TAG + "\tcommandline\tgiven=" + given + "\tprofiles=" + profiles);
		if (given)
		{
			List("cmdline-json-dirs", profiles + "/VyshkaFind/*.json", FindFileFlags.DIRECTORIES);
			List("cmdline-json-all", profiles + "/VyshkaFind/*.json", FindFileFlags.ALL);
			List("cmdline-star-dirs", profiles + "/*", FindFileFlags.DIRECTORIES);
			// Read and write through the plain path as well, to know whether
			// a path without the alias may be opened at all.
			FileHandle plain = OpenFile(profiles + "/VyshkaFind/1.json", FileMode.READ);
			Print(TAG + "\tplainOpen\topened=" + (plain != 0) + "\texists=" + FileExist(profiles + "/VyshkaFind/1.json"));
			if (plain != 0)
				CloseFile(plain);
		}
		List("cwd-mpmissions-dirs", "mpmissions/*", FindFileFlags.DIRECTORIES);
		Print(TAG + "\tfinished");
	}

	static void Write(string name, string text)
	{
		FileHandle file = OpenFile(DIR + "/" + name, FileMode.WRITE);
		if (file == 0)
		{
			Print(TAG + "\terror\tcannot open " + name);
			return;
		}
		FPrint(file, text);
		CloseFile(file);
	}

	static void List(string label, string pattern, FindFileFlags flags)
	{
		string fileName;
		FileAttr attributes;
		FindFileHandle handle = FindFile(pattern, fileName, attributes, flags);
		if (!handle)
		{
			Print(TAG + "\tlist\t" + label + "\thandle=false\tpattern=" + pattern);
			return;
		}
		string names = "[" + fileName + "|" + attributes + "]";
		int count = 1;
		while (count < 64 && FindNextFile(handle, fileName, attributes))
		{
			names += " [" + fileName + "|" + attributes + "]";
			count++;
		}
		CloseFindFile(handle);
		Print(TAG + "\tlist\t" + label + "\thandle=true\tcount=" + count + "\tpattern=" + pattern + "\tnames=" + names);
	}
}
