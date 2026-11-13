// Vyshka Reforger plugin (protocol code shared with the DayZ plugin): logging.
//
// Everything goes through Print, which lands in the server's console log
// (<profile>/logs/<timestamp>/console.log).
// A fixed prefix keeps the lines greppable; spec section 6.4 asks that a
// manifest.reject reach the operator as at least a log line, and this is it.

class VyshkaLog
{
	static const string PREFIX = "[Vyshka] ";

	static void Info(string message)
	{
		Print(PREFIX + message);
	}

	static void Warn(string message)
	{
		Print(PREFIX + "WARNING: " + message);
	}

	static void Error(string message)
	{
		Print(PREFIX + "ERROR: " + message);
	}
}
