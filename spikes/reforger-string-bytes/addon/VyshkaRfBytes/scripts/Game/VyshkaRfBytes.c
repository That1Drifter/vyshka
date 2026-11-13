// Vyshka: how Reforger's string functions index text that holds multi-byte
// UTF-8. The JSON and text code the Reforger plugin shares with the DayZ
// plugin assumes byte indexing. Each result is one line in the console log:
//
//   VYSHKA_BYTES <test> <value>

class VyshkaRfBytes
{
	static void Out(string test, string value)
	{
		Print("VYSHKA_BYTES\t" + test + "\t" + value, LogLevel.NORMAL);
	}

	static void Run()
	{
		string e = "é";     // U+00E9, two bytes C3 A9
		string zh = "中";   // U+4E2D, three bytes E4 B8 AD
		string mixed = "a" + e + "b" + zh + "c";
		Out("e.Length", e.Length().ToString());
		Out("zh.Length", zh.Length().ToString());
		Out("mixed.Length", mixed.Length().ToString());
		for (int i = 0; i < mixed.Length(); i++)
		{
			string g = mixed.Get(i);
			Out("mixed.ToAscii(" + i.ToString() + ")", mixed.ToAscii(i).ToString());
			Out("mixed.Get(" + i.ToString() + ").Length", g.Length().ToString());
			Out("mixed.Get(" + i.ToString() + ").ToAscii", g.ToAscii().ToString());
		}
		string sub = mixed.Substring(1, 2);
		Out("mixed.Substring(1,2)==e", (sub == e).ToString());
		Out("mixed.Substring(1,2).Length", sub.Length().ToString());
		string sub1 = mixed.Substring(1, 1);
		Out("mixed.Substring(1,1).Length", sub1.Length().ToString());
		string rebuilt = mixed.Get(1) + mixed.Get(2);
		Out("Get(1)+Get(2)==e", (rebuilt == e).ToString());
		Out("IndexOf(b)", mixed.IndexOf("b").ToString());
		Out("IndexOf(c)", mixed.IndexOf("c").ToString());
		int c3 = 195;
		int a9 = 169;
		string fromCodes = c3.AsciiToString() + a9.AsciiToString();
		Out("AsciiToString(195)+(169)==e", (fromCodes == e).ToString());
		Out("AsciiToString(195).Length", c3.AsciiToString().Length().ToString());
		Out("done", "ok");
	}
}

modded class SCR_BaseGameMode
{
	protected override void OnGameStart()
	{
		super.OnGameStart();
		if (Replication.IsServer())
			VyshkaRfBytes.Run();
	}
}
