package webhook

import "testing"

func TestDiscordDamageReadsLikeAHitLog(t *testing.T) {
	t.Parallel()
	rendered := renderForTest(t, "core.player.damage", `{
		"player": {"platform": "steam", "id": "1"}, "name": "Survivor",
		"attacker": {"platform": "steam", "id": "2"}, "attackerName": "Raider",
		"cause": "player", "weapon": "M4-A1", "distance": 312.4, "damageType": "firearm",
		"bodyPart": "Torso", "ammo": "Bullet_556x45", "damage": 34.26, "blood": 120, "shock": 40,
		"health": 65.74, "position": [4231.5, 300.2, 10620.0]}`, "Chernarus 1")
	embed := rendered.Embeds[0]
	if embed.Title != "Player hit" {
		t.Errorf("title = %q, want Player hit", embed.Title)
	}
	if want := "**Survivor** was hit by **Raider** with M4-A1 from 312 m in the Torso for 34.3 damage (Bullet\\_556x45)"; embed.Description != want {
		t.Errorf("description = %q, want %q", embed.Description, want)
	}
	if len(embed.Fields) != 2 || embed.Fields[0].Name != "Health left" || embed.Fields[0].Value != "66" || embed.Fields[1].Name != "Position" || embed.Fields[1].Value != "4232, 300, 10620" {
		t.Errorf("fields = %+v, want Health left 66 and the position", embed.Fields)
	}
	if embed.Color != discordOrange {
		t.Errorf("color = %#x, want orange", embed.Color)
	}

	infected := renderForTest(t, "core.player.damage", `{"name": "Survivor", "cause": "infected", "sourceType": "ZmbM_HunterOld_Summer",
		"damageType": "melee", "bodyPart": "LeftLeg", "ammo": "MeleeInfectedLong", "damage": 7.65, "health": 92.09}`, "")
	if want := "**Survivor** was hit by an infected in the LeftLeg for 7.7 damage (MeleeInfectedLong)"; infected.Embeds[0].Description != want {
		t.Errorf("infected hit = %q, want %q", infected.Embeds[0].Description, want)
	}

	fatal := renderForTest(t, "core.player.damage", `{"name": "Survivor", "cause": "player", "attacker": {"platform": "steam", "id": "2"},
		"bodyPart": "Brain", "ammo": "Bullet_556x45", "damage": 100, "health": 0, "fatal": true}`, "")
	if fatal.Embeds[0].Title != "Player hit (fatal)" || fatal.Embeds[0].Color != discordRed {
		t.Errorf("fatal hit title = %q color %#x, want the fatal title in red", fatal.Embeds[0].Title, fatal.Embeds[0].Color)
	}
	if want := "**Survivor** was hit by **steam:2** in the Brain for 100.0 damage (Bullet\\_556x45)"; fatal.Embeds[0].Description != want {
		t.Errorf("fatal hit = %q, want %q", fatal.Embeds[0].Description, want)
	}

	blocked := renderForTest(t, "core.player.damage", `{"name": "Survivor", "cause": "self", "ammo": "FallDamageHealth", "blocked": true, "damage": 0, "health": 100}`, "")
	if want := "**Survivor** was hurt, blocked (FallDamageHealth)"; blocked.Embeds[0].Description != want {
		t.Errorf("blocked hit = %q, want %q", blocked.Embeds[0].Description, want)
	}

	bare := renderForTest(t, "core.player.damage", `{"player": {"platform": "steam", "id": "7"}}`, "")
	if bare.Embeds[0].Description != "**steam:7** was hit" {
		t.Errorf("bare hit = %q", bare.Embeds[0].Description)
	}
	if len(bare.Embeds[0].Fields) != 0 {
		t.Errorf("bare hit fields = %+v, want none", bare.Embeds[0].Fields)
	}
}

func TestDiscordDeathCarriesTheHitAndTheVitals(t *testing.T) {
	t.Parallel()
	headshot := renderForTest(t, "core.player.death", `{"name": "Survivor", "cause": "player", "killerName": "Raider",
		"weapon": "M4-A1", "distance": 40, "bodyPart": "Brain", "ammo": "Bullet_556x45", "damageType": "firearm"}`, "")
	embed := headshot.Embeds[0]
	if want := "**Survivor** was killed by **Raider** with M4-A1 from 40 m"; embed.Description != want {
		t.Errorf("description = %q, want %q", embed.Description, want)
	}
	if len(embed.Fields) != 1 || embed.Fields[0].Name != "Hit" || embed.Fields[0].Value != "Brain (Bullet\\_556x45)" {
		t.Errorf("fields = %+v, want one Hit field", embed.Fields)
	}

	starved := renderForTest(t, "core.player.death", `{"name": "Survivor", "cause": "self",
		"water": 812.4, "energy": 0, "blood": 4980.5, "bleedingSources": 0, "submerged": false}`, "")
	if starved.Embeds[0].Description != "**Survivor** died" {
		t.Errorf("starved = %q", starved.Embeds[0].Description)
	}
	if len(starved.Embeds[0].Fields) != 1 || starved.Embeds[0].Fields[0].Name != "At death" || starved.Embeds[0].Fields[0].Value != "water 812, energy 0, blood 4980, bleeding sources 0" {
		t.Errorf("fields = %+v, want one At death field", starved.Embeds[0].Fields)
	}

	// The head under water is listed with the vitals, never worded as a
	// drowning: the plugin observes submersion, it does not rule on the cause.
	submerged := renderForTest(t, "core.player.death", `{"name": "Survivor", "cause": "self", "submerged": true, "water": 1000, "energy": 4960, "bleedingSources": 0}`, "")
	if submerged.Embeds[0].Description != "**Survivor** died" {
		t.Errorf("submerged death = %q", submerged.Embeds[0].Description)
	}
	if len(submerged.Embeds[0].Fields) != 1 || submerged.Embeds[0].Fields[0].Value != "water 1000, energy 4960, bleeding sources 0, submerged" {
		t.Errorf("submerged fields = %+v", submerged.Embeds[0].Fields)
	}

	wire := renderForTest(t, "core.player.death", `{"name": "Survivor", "cause": "other", "killerType": "BarbedWire", "ammo": "BarbedWireHit"}`, "")
	if got := wire.Embeds[0].Description; got != "**Survivor** was killed by BarbedWire" {
		t.Errorf("wire death = %q", got)
	}
	if len(wire.Embeds[0].Fields) != 1 || wire.Embeds[0].Fields[0].Value != "BarbedWireHit" {
		t.Errorf("wire fields = %+v, want the ammunition as the Hit field", wire.Embeds[0].Fields)
	}
}
