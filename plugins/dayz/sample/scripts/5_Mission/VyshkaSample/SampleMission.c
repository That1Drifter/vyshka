// Vyshka sample mod: registration.
//
// One hook is the whole of it. MissionServer.VyshkaRegister runs once, before
// the plugin starts, so everything registered here is in the first manifest
// the hub sees. super comes first, or the plugin's own actions are lost.
//
// The #else branch is the point of the #ifdef: the same PBO loads on a server
// that is not running Vyshka, says so once, and does nothing else.

#ifdef VYSHKA

modded class MissionServer
{
	override void VyshkaRegister(VyshkaRegistry registry)
	{
		super.VyshkaRegister(registry);
		registry.DeclareNamespace("sample");
		registry.RegisterContext(new SampleLandmarkContext());
		registry.DeclareEvent("sample.beacon.placed", "Beacon placed", "sample", SampleBeaconAction.PlacedSchema());
		registry.Register(new SampleBeaconAction());
		// Each call goes through the local: a method called on an object
		// another call just returned runs on an instance the engine may
		// already have released.
		VyshkaLink link = GetVyshka();
		string version = link.Version();
		string state = link.LinkState();
		Print("[VyshkaSample] registered with Vyshka " + version + ", link " + state);
	}
}

#else

modded class MissionServer
{
	override void OnInit()
	{
		super.OnInit();
		Print("[VyshkaSample] loaded without Vyshka; nothing to register");
	}
}

#endif
