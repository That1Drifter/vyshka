void main()
{
	//INIT ECONOMY--------------------------------------
	Hive ce = CreateHive();
	if ( ce )
		ce.InitOffline();

	//DATE RESET AFTER ECONOMY INIT-------------------------
	int year, month, day, hour, minute;
	int reset_month = 9, reset_day = 20;
	GetGame().GetWorld().GetDate(year, month, day, hour, minute);

	if ((month == reset_month) && (day < reset_day))
	{
		GetGame().GetWorld().SetDate(year, reset_month, reset_day, hour, minute);
	}
	else
	{
		if ((month == reset_month + 1) && (day > reset_day))
		{
			GetGame().GetWorld().SetDate(year, reset_month, reset_day, hour, minute);
		}
		else
		{
			if ((month < reset_month) || (month > reset_month + 1))
			{
				GetGame().GetWorld().SetDate(year, reset_month, reset_day, hour, minute);
			}
		}
	}
}

class CustomMission: MissionServer
{
	// Vyshka admin flags live-test rig (issue #71), test mission only, not
	// part of the plugin. Trigger files under $profile: act on every alive
	// player and log VYSHKA_RIG lines:
	//   vyshka-rig-shot.txt        a firearm hit from a rifle lying on the ground
	//                              (the engine's direct damage call): does god mode hold
	//   vyshka-rig-infected.txt    spawn an infected with AI 3 m in front of the player
	//   vyshka-rig-melee.txt       the engine's close-combat call from that infected
	//   vyshka-rig-report.txt      position, health, allow-damage, stamina, interaction
	//                              layer, the flags the plugin holds, the current magazine
	//   vyshka-rig-invisible.txt   the spike: SetInvisible(true) on the character
	//   vyshka-rig-visible.txt     SetInvisible(false)
	//   vyshka-rig-nocollision.txt the spike: the character's physics body to the
	//                              NOCOLLISION interaction layer
	//   vyshka-rig-collision.txt   back to the CHARACTER layer
	//   vyshka-rig-clearhands.txt  delete whatever the player holds (the rifle needs empty hands)
	//   vyshka-rig-rifle.txt       give the player an M4A1 with a full magazine in hand
	//   vyshka-rig-fire.txt        take one round from the held weapon's magazine and run
	//                              its fire event, as the server sees a shot
	float m_VyshkaRigAccum;
	EntityAI m_VyshkaRigInfected;
	EntityAI m_VyshkaRigRifle;

	override void OnInit()
	{
		super.OnInit();
		Print("VYSHKA_RIG\tarmed");
	}

	override void OnUpdate(float timeslice)
	{
		super.OnUpdate(timeslice);
		m_VyshkaRigAccum += timeslice;
		if (m_VyshkaRigAccum < 1.0)
			return;
		m_VyshkaRigAccum = 0;
		VyshkaRigCheck("shot");
		VyshkaRigCheck("infected");
		VyshkaRigCheck("melee");
		VyshkaRigCheck("report");
		VyshkaRigCheck("invisible");
		VyshkaRigCheck("visible");
		VyshkaRigCheck("nocollision");
		VyshkaRigCheck("collision");
		VyshkaRigCheck("clearhands");
		VyshkaRigCheck("rifle");
		VyshkaRigCheck("fire");
	}

	void VyshkaRigCheck(string mode)
	{
		string path = "$profile:vyshka-rig-" + mode + ".txt";
		if (!FileExist(path))
			return;
		DeleteFile(path);
		VyshkaRig(mode);
	}

	void VyshkaRig(string mode)
	{
		array<Man> men = new array<Man>;
		GetGame().GetPlayers(men);
		for (int i = 0; i < men.Count(); i++)
		{
			PlayerBase p = PlayerBase.Cast(men.Get(i));
			if (!p || !p.IsAlive())
				continue;
			vector pos = p.GetPosition();
			Print("VYSHKA_RIG\t" + mode + " on " + p.GetIdentity().GetName());
			if (mode == "report")
			{
				string flags = "none";
				if (p.m_VyshkaFlags)
				{
					VyshkaJsonValue flagsJson = p.m_VyshkaFlags.ToJson();
					flags = flagsJson.Serialize();
				}
				string mag = "no weapon in hands";
				Weapon_Base weapon = Weapon_Base.Cast(p.GetHumanInventory().GetEntityInHands());
				if (weapon)
				{
					Magazine magazine = weapon.GetMagazine(weapon.GetCurrentMuzzle());
					if (magazine)
						mag = weapon.GetType() + " magazine " + magazine.GetAmmoCount().ToString() + "/" + magazine.GetAmmoMax().ToString();
					else
						mag = weapon.GetType() + " internal " + weapon.GetInternalMagazineCartridgeCount(weapon.GetCurrentMuzzle()).ToString() + "/" + weapon.GetInternalMagazineMaxCartridgeCount(weapon.GetCurrentMuzzle()).ToString();
				}
				float stamina = -1;
				if (p.GetStaminaHandler())
					stamina = p.GetStaminaHandler().GetStamina();
				Print("VYSHKA_RIG\treport pos=" + pos.ToString() + " health=" + p.GetHealth("", "Health").ToString() + " allowDamage=" + p.GetAllowDamage().ToString() + " stamina=" + stamina.ToString() + " layer=" + dBodyGetInteractionLayer(p).ToString() + " flags=" + flags + " " + mag);
			}
			else if (mode == "shot")
			{
				if (!m_VyshkaRigRifle)
					m_VyshkaRigRifle = EntityAI.Cast(GetGame().CreateObjectEx("M4A1", pos + "0 0 3", ECE_PLACE_ON_SURFACE));
				float hpBefore = p.GetHealth("", "Health");
				if (m_VyshkaRigRifle)
					p.ProcessDirectDamage(DamageType.FIRE_ARM, m_VyshkaRigRifle, "Torso", "Bullet_556x45", pos, 1.0);
				Print("VYSHKA_RIG\tshot health " + hpBefore.ToString() + " -> " + p.GetHealth("", "Health").ToString() + " alive " + p.IsAlive().ToString() + " allowDamage " + p.GetAllowDamage().ToString());
			}
			else if (mode == "infected")
			{
				vector at = pos + p.GetDirection() * 3;
				m_VyshkaRigInfected = EntityAI.Cast(GetGame().CreateObjectEx("ZmbM_HunterOld_Summer", at, ECE_PLACE_ON_SURFACE | ECE_INITAI | ECE_EQUIP_ATTACHMENTS));
				if (m_VyshkaRigInfected)
					Print("VYSHKA_RIG\tinfected created at " + m_VyshkaRigInfected.GetPosition().ToString() + " alive " + m_VyshkaRigInfected.IsAlive().ToString() + " canTarget " + p.CanBeTargetedByAI(m_VyshkaRigInfected).ToString());
				else
					Print("VYSHKA_RIG\tinfected creation failed");
			}
			else if (mode == "melee")
			{
				if (!m_VyshkaRigInfected)
				{
					Print("VYSHKA_RIG\tno infected to hit with");
					continue;
				}
				float before = p.GetHealth("", "Health");
				p.ProcessDirectDamage(DamageType.CLOSE_COMBAT, m_VyshkaRigInfected, "Head", "MeleeInfected", pos, 1.0);
				Print("VYSHKA_RIG\tmelee health " + before.ToString() + " -> " + p.GetHealth("", "Health").ToString());
			}
			else if (mode == "invisible")
			{
				p.SetInvisible(true);
				Print("VYSHKA_RIG\tSetInvisible(true) called");
			}
			else if (mode == "visible")
			{
				p.SetInvisible(false);
				Print("VYSHKA_RIG\tSetInvisible(false) called");
			}
			else if (mode == "nocollision")
			{
				int layerBefore = dBodyGetInteractionLayer(p);
				dBodySetInteractionLayer(p, PhxInteractionLayers.NOCOLLISION);
				Print("VYSHKA_RIG\tinteraction layer " + layerBefore.ToString() + " -> " + dBodyGetInteractionLayer(p).ToString());
			}
			else if (mode == "collision")
			{
				int layerWas = dBodyGetInteractionLayer(p);
				dBodySetInteractionLayer(p, PhxInteractionLayers.CHARACTER);
				Print("VYSHKA_RIG\tinteraction layer " + layerWas.ToString() + " -> " + dBodyGetInteractionLayer(p).ToString());
			}
			else if (mode == "fire")
			{
				// A shot's server-side aftermath without a client firing: one
				// round taken from the magazine, then the weapon's own fire
				// event, the hook the plugin refills from.
				Weapon_Base held = Weapon_Base.Cast(p.GetHumanInventory().GetEntityInHands());
				if (!held)
				{
					Print("VYSHKA_RIG\tno weapon in hands to fire");
					continue;
				}
				int muzzleIndex = held.GetCurrentMuzzle();
				Magazine heldMag = held.GetMagazine(muzzleIndex);
				if (!heldMag)
				{
					Print("VYSHKA_RIG\tno magazine attached");
					continue;
				}
				int roundsBefore = heldMag.GetAmmoCount();
				heldMag.ServerSetAmmoCount(roundsBefore - 1);
				int roundsAfterTake = heldMag.GetAmmoCount();
				held.EEFired(muzzleIndex, 0, "Bullet_556x45");
				Print("VYSHKA_RIG\tfire: magazine " + roundsBefore.ToString() + " -> " + roundsAfterTake.ToString() + " after the round taken -> " + heldMag.GetAmmoCount().ToString() + " after EEFired");
			}
			else if (mode == "clearhands")
			{
				// The deletion is deferred by the engine, so the rifle is a
				// separate trigger a tick later.
				EntityAI inHands = p.GetHumanInventory().GetEntityInHands();
				if (inHands)
				{
					Print("VYSHKA_RIG\tdeleting " + inHands.GetType() + " from hands");
					GetGame().ObjectDelete(inHands);
				}
				else
					Print("VYSHKA_RIG\thands already empty");
			}
			else if (mode == "rifle")
			{
				EntityAI rifle = p.GetHumanInventory().CreateInHands("M4A1");
				if (rifle)
				{
					EntityAI attached = rifle.GetInventory().CreateAttachment("Mag_STANAG_30Rnd");
					Magazine full = Magazine.Cast(attached);
					if (full)
						full.ServerSetAmmoMax();
					Print("VYSHKA_RIG\trifle in hands, magazine attached " + (full != null).ToString());
				}
				else
					Print("VYSHKA_RIG\trifle could not be created in hands");
			}
		}
	}

	void SetRandomHealth(EntityAI itemEnt)
	{
		if ( itemEnt )
		{
			float rndHlt = Math.RandomFloat( 0.45, 0.65 );
			itemEnt.SetHealth01( "", "", rndHlt );
		}
	}

	override PlayerBase CreateCharacter(PlayerIdentity identity, vector pos, ParamsReadContext ctx, string characterName)
	{
		Entity playerEnt;
		playerEnt = GetGame().CreatePlayer( identity, characterName, pos, 0, "NONE" );
		Class.CastTo( m_player, playerEnt );

		GetGame().SelectPlayer( identity, m_player );

		return m_player;
	}

	override void StartingEquipSetup(PlayerBase player, bool clothesChosen)
	{
		EntityAI itemClothing;
		EntityAI itemEnt;
		ItemBase itemBs;
		float rand;

		itemClothing = player.FindAttachmentBySlotName( "Body" );
		if ( itemClothing )
		{
			SetRandomHealth( itemClothing );

			itemEnt = itemClothing.GetInventory().CreateInInventory( "BandageDressing" );
			player.SetQuickBarEntityShortcut(itemEnt, 2);

			string chemlightArray[] = { "Chemlight_White", "Chemlight_Yellow", "Chemlight_Green", "Chemlight_Red" };
			int rndIndex = Math.RandomInt( 0, 4 );
			itemEnt = itemClothing.GetInventory().CreateInInventory( chemlightArray[rndIndex] );
			SetRandomHealth( itemEnt );
			player.SetQuickBarEntityShortcut(itemEnt, 1);

			rand = Math.RandomFloatInclusive( 0.0, 1.0 );
			if ( rand < 0.35 )
				itemEnt = player.GetInventory().CreateInInventory( "Apple" );
			else if ( rand > 0.65 )
				itemEnt = player.GetInventory().CreateInInventory( "Pear" );
			else
				itemEnt = player.GetInventory().CreateInInventory( "Plum" );
			player.SetQuickBarEntityShortcut(itemEnt, 3);
			SetRandomHealth( itemEnt );
		}

		itemClothing = player.FindAttachmentBySlotName( "Legs" );
		if ( itemClothing )
			SetRandomHealth( itemClothing );

		itemClothing = player.FindAttachmentBySlotName( "Feet" );
	}
};

Mission CreateCustomMission(string path)
{
	return new CustomMission();
}
