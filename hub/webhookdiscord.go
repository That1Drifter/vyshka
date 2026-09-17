package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/That1Drifter/vyshka/hub/store"
)

// The discord webhook template (spec section 11.3): a delivery rendered as a
// Discord webhook execution body, one embed per notification, so a channel
// can show a kill feed or a moderation log with no bridge in between.
//
// Everything a player can type (a name, a chat line, a reason) is escaped
// before it reaches the embed, so it cannot inject formatting, and every
// mention is disabled through allowed_mentions, so it cannot ping anyone.
// Field lengths are cut to Discord's documented limits. The body is rendered
// once at fan-out like generic-json, so retries stay byte-identical and the
// section 11.4 signature holds; Discord ignores the extra headers.

const templateDiscord = "discord"

// Discord's documented limits for a webhook execution body.
const (
	discordTitleMax       = 256
	discordDescriptionMax = 4096
	discordFieldNameMax   = 256
	discordFieldValueMax  = 1024
	discordFooterMax      = 2048
	discordFieldsMax      = 25
	discordGenericFields  = 10
	discordEmbedTotalMax  = 6000
	discordUsername       = "Vyshka"
)

// Embed colors, RGB.
const (
	discordGreen  = 0x2ECC71
	discordRed    = 0xE74C3C
	discordOrange = 0xE67E22
	discordBlue   = 0x3498DB
	discordGrey   = 0x95A5A6
	discordDark   = 0x992D22
)

type discordBody struct {
	Username        string          `json:"username"`
	Embeds          []discordEmbed  `json:"embeds"`
	AllowedMentions discordMentions `json:"allowed_mentions"`
}

// discordMentions with an empty parse list tells Discord to resolve no
// mention at all, whatever the text says.
type discordMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Timestamp   string         `json:"timestamp"`
	Footer      discordFooter  `json:"footer"`
	Fields      []discordField `json:"fields,omitempty"`
}

type discordFooter struct {
	Text string `json:"text"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// renderDiscord builds the delivery body for one notification. serverName is
// the server's display name, or "" when unknown, in which case the id stands
// in. It never fails: a payload the renderer cannot read produces the generic
// embed rather than no delivery.
func renderDiscord(one notification, serverName string) ([]byte, error) {
	var data map[string]any
	if len(one.Data) > 0 {
		// Numbers stay json.Number: an id above 2^53 in a custom payload
		// must render as written, not rounded through a float64.
		decoder := json.NewDecoder(bytes.NewReader(one.Data))
		decoder.UseNumber()
		_ = decoder.Decode(&data)
	}
	embed := discordEmbedFor(one.Type, data)
	embed.Timestamp = envelopeTimestamp(one.OccurredAt)
	if serverName == "" {
		serverName = one.ServerID
	}
	embed.Footer = discordFooter{Text: clipText(serverName+" · "+one.Type, discordFooterMax)}
	embed = boundDiscordEmbed(embed)
	return json.Marshal(discordBody{
		Username:        discordUsername,
		Embeds:          []discordEmbed{embed},
		AllowedMentions: discordMentions{Parse: []string{}},
	})
}

// discordEmbedFor renders the embed's words for a notification type. The
// timestamp and footer are added by the caller.
func discordEmbedFor(notificationType string, data map[string]any) discordEmbed {
	switch notificationType {
	case "core.player.connect":
		return discordEmbed{Title: "Player connected", Description: playerLabel(data) + " joined", Color: discordGreen}
	case "core.player.disconnect":
		return discordEmbed{Title: "Player disconnected", Description: playerLabel(data) + " left", Color: discordGrey}
	case "core.player.death":
		return discordDeath(data)
	case "core.player.damage":
		return discordDamage(data)
	case "core.player.chat":
		channel := stringField(data, "channel")
		title := "Chat"
		if channel != "" {
			title = "Chat (" + escapeMarkdown(channel) + ")"
		}
		return discordEmbed{Title: title, Description: playerLabel(data) + ": " + escapeMarkdown(stringField(data, "text")), Color: discordBlue}
	case "core.player.kick":
		description := playerLabel(data) + " was kicked"
		if stringField(data, "cause") == "ban" {
			description = playerLabel(data) + " was refused: banned"
		}
		if reason := stringField(data, "reason"); reason != "" {
			description += " (" + escapeMarkdown(reason) + ")"
		}
		return discordEmbed{Title: "Player kicked", Description: description, Color: discordOrange, Fields: actionIDField(data)}
	case "core.player.ban":
		description := playerLabel(data) + " was banned"
		if expires := stringField(data, "expiresAt"); expires != "" {
			description += " until " + escapeMarkdown(expires)
		}
		if reason := stringField(data, "reason"); reason != "" {
			description += " (" + escapeMarkdown(reason) + ")"
		}
		return discordEmbed{Title: "Player banned", Description: description, Color: discordDark, Fields: actionIDField(data)}
	case "core.server.start":
		return discordEmbed{Title: "Server started", Description: serverDescription(data), Color: discordGreen}
	case "core.server.stop":
		return discordEmbed{Title: "Server stopped", Description: serverDescription(data), Color: discordGrey}
	case "core.server.fps":
		description := ""
		if fps, ok := numberField(data, "fps"); ok {
			description = strconv.FormatFloat(fps, 'f', 1, 64) + " fps"
		}
		if players, ok := numberField(data, "players"); ok {
			if description != "" {
				description += ", "
			}
			description += plural(int(players), "player")
		}
		if description == "" {
			description = "no sample"
		}
		return discordEmbed{Title: "Performance", Description: description, Color: discordBlue}
	case notifyActionCompleted:
		return discordAction(data)
	case notifyServerLinkLost:
		return discordEmbed{Title: "Server link lost", Description: lastSeenDescription(data), Color: discordRed}
	case notifyServerLinkRestore:
		return discordEmbed{Title: "Server link restored", Description: lastSeenDescription(data), Color: discordGreen}
	}
	return discordGeneric(notificationType, data)
}

// discordDeath words a death the way a kill feed does, from the payload the
// reference plugin publishes: cause, and where known the killer, weapon,
// distance, and killer type; the hit that killed and the vitals at a death
// the engine names the character itself as the killer of become fields. A
// payload without those words still reads.
func discordDeath(data map[string]any) discordEmbed {
	victim := playerLabel(data)
	weapon := escapeMarkdown(stringField(data, "weapon"))
	var description string
	switch stringField(data, "cause") {
	case "player":
		description = victim + " was killed by " + otherPlayerLabel(data, "killerName", "killer")
		if weapon != "" {
			description += " with " + weapon
		}
		if distance, ok := numberField(data, "distance"); ok {
			description += " from " + strconv.FormatFloat(distance, 'f', 0, 64) + " m"
		}
	case "self":
		description = victim + " died"
		if weapon != "" {
			description += " to their own " + weapon
		}
	case "infected":
		description = victim + " was killed by an infected"
	case "animal":
		description = victim + " was killed by an animal"
	case "explosion":
		description = victim + " was killed by an explosion"
		if weapon != "" {
			description += " (" + weapon + ")"
		}
	case "vehicle":
		description = victim + " was killed by a vehicle"
	case "other":
		description = victim + " was killed"
		if killerType := escapeMarkdown(stringField(data, "killerType")); killerType != "" {
			description += " by " + killerType
		}
	default:
		description = victim + " died"
	}
	embed := discordEmbed{Title: "Player died", Description: description, Color: discordRed}
	embed.Fields = append(embed.Fields, positionField(data)...)
	if hit := hitLabel(data); hit != "" {
		embed.Fields = append(embed.Fields, discordField{Name: "Hit", Value: hit, Inline: true})
	}
	vitals := make([]string, 0, 4)
	for _, stat := range []string{"water", "energy", "blood"} {
		if value, ok := numberField(data, stat); ok {
			vitals = append(vitals, stat+" "+strconv.FormatFloat(value, 'f', 0, 64))
		}
	}
	if sources, ok := numberField(data, "bleedingSources"); ok {
		vitals = append(vitals, "bleeding sources "+strconv.FormatFloat(sources, 'f', 0, 64))
	}
	// The head under water is an observation the plugin makes, not a
	// verdict on the cause (a submerged character can bleed out), so it is
	// listed with the vitals rather than worded as a drowning.
	if submerged, _ := data["submerged"].(bool); submerged {
		vitals = append(vitals, "submerged")
	}
	if len(vitals) > 0 {
		embed.Fields = append(embed.Fields, discordField{Name: "At death", Value: strings.Join(vitals, ", "), Inline: true})
	}
	return embed
}

// discordDamage words one hit from the payload the reference plugin
// publishes: cause and the attacker as for a death, then the body part, the
// damage, and the ammunition; the health left is a field, and a fatal hit
// says so in the title.
func discordDamage(data map[string]any) discordEmbed {
	victim := playerLabel(data)
	weapon := escapeMarkdown(stringField(data, "weapon"))
	var description string
	switch stringField(data, "cause") {
	case "player":
		description = victim + " was hit by " + otherPlayerLabel(data, "attackerName", "attacker")
		if weapon != "" {
			description += " with " + weapon
		}
		if distance, ok := numberField(data, "distance"); ok {
			description += " from " + strconv.FormatFloat(distance, 'f', 0, 64) + " m"
		}
	case "self":
		description = victim + " was hurt"
		if weapon != "" {
			description += " by their own " + weapon
		}
	case "infected":
		description = victim + " was hit by an infected"
	case "animal":
		description = victim + " was hit by an animal"
	case "explosion":
		description = victim + " was hit by an explosion"
		if weapon != "" {
			description += " (" + weapon + ")"
		}
	case "vehicle":
		description = victim + " was hit by a vehicle"
	case "other":
		description = victim + " was hit"
		if sourceType := escapeMarkdown(stringField(data, "sourceType")); sourceType != "" {
			description += " by " + sourceType
		}
	default:
		description = victim + " was hit"
	}
	if bodyPart := escapeMarkdown(stringField(data, "bodyPart")); bodyPart != "" {
		description += " in the " + bodyPart
	}
	if blocked, _ := data["blocked"].(bool); blocked {
		description += ", blocked"
	} else if damage, ok := numberField(data, "damage"); ok {
		description += " for " + strconv.FormatFloat(damage, 'f', 1, 64) + " damage"
	}
	if ammo := escapeMarkdown(stringField(data, "ammo")); ammo != "" {
		description += " (" + ammo + ")"
	}
	embed := discordEmbed{Title: "Player hit", Description: description, Color: discordOrange}
	if fatal, _ := data["fatal"].(bool); fatal {
		embed.Title = "Player hit (fatal)"
		embed.Color = discordRed
	}
	if health, ok := numberField(data, "health"); ok {
		embed.Fields = append(embed.Fields, discordField{Name: "Health left", Value: strconv.FormatFloat(health, 'f', 0, 64), Inline: true})
	}
	embed.Fields = append(embed.Fields, positionField(data)...)
	return embed
}

// otherPlayerLabel is the bold, escaped name of the other player a death or
// hit payload names under nameKey, the identity under identityKey when there
// is no name, and "another player" when there is neither.
func otherPlayerLabel(data map[string]any, nameKey, identityKey string) string {
	other := escapeMarkdown(stringField(data, nameKey))
	if other == "" {
		other = identityLabel(data[identityKey])
	}
	if other == "" {
		return "another player"
	}
	return "**" + other + "**"
}

// hitLabel renders the body part and ammunition of the hit that killed, when
// the payload carries them: "Brain (Bullet_556x45)".
func hitLabel(data map[string]any) string {
	bodyPart := escapeMarkdown(stringField(data, "bodyPart"))
	ammo := escapeMarkdown(stringField(data, "ammo"))
	switch {
	case bodyPart != "" && ammo != "":
		return bodyPart + " (" + ammo + ")"
	case bodyPart != "":
		return bodyPart
	default:
		return ammo
	}
}

// positionField renders a [x, y, z] position as one inline field, or none.
func positionField(data map[string]any) []discordField {
	position, ok := data["position"].([]any)
	if !ok || len(position) != 3 {
		return nil
	}
	parts := make([]string, 0, 3)
	for _, component := range position {
		if value, ok := numberValue(component); ok {
			parts = append(parts, strconv.FormatFloat(value, 'f', 0, 64))
		}
	}
	if len(parts) != 3 {
		return nil
	}
	return []discordField{{Name: "Position", Value: strings.Join(parts, ", "), Inline: true}}
}

// discordAction words an action.completed notification from the record it
// carries (section 11.1).
func discordAction(data map[string]any) discordEmbed {
	code := escapeMarkdown(stringField(data, "code"))
	if code == "" {
		code = "an action"
	}
	state := stringField(data, "state")
	embed := discordEmbed{Fields: actionIDField(data)}
	switch state {
	case "completed":
		embed.Title = "Action completed"
		embed.Description = code + " completed"
		if duration, ok := numberField(data, "durationMs"); ok {
			embed.Description += " in " + strconv.FormatFloat(duration, 'f', 0, 64) + " ms"
		}
		embed.Color = discordGreen
	case "failed":
		embed.Title = "Action failed"
		embed.Description = code + " failed"
		if reason := escapeMarkdown(stringField(data, "error")); reason != "" {
			embed.Description += ": " + reason
		}
		embed.Color = discordRed
	case "expired":
		embed.Title = "Action expired"
		embed.Description = code + " expired before the server answered"
		embed.Color = discordOrange
	default:
		embed.Title = "Action " + escapeMarkdown(state)
		embed.Description = code
		embed.Color = discordGrey
	}
	return embed
}

// discordGeneric is the embed for a type the hub has no words for: the type
// as the title and the payload's top-level members as fields, in key order,
// so a custom event still reads in the channel.
func discordGeneric(notificationType string, data map[string]any) discordEmbed {
	embed := discordEmbed{Title: escapeMarkdown(notificationType), Color: discordGrey}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(embed.Fields) == discordGenericFields {
			embed.Fields = append(embed.Fields, discordField{
				Name:  "…",
				Value: plural(len(keys)-discordGenericFields, "more member") + " not shown",
			})
			break
		}
		embed.Fields = append(embed.Fields, discordField{
			Name:   clipText(escapeMarkdown(key), discordFieldNameMax),
			Value:  clipText(scalarText(data[key]), discordFieldValueMax),
			Inline: true,
		})
	}
	if len(embed.Fields) == 0 {
		embed.Description = "no payload"
	}
	return embed
}

// scalarText renders one payload member for a field: strings escaped,
// numbers and booleans as written, anything nested as compact JSON.
func scalarText(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		if typed == "" {
			return "(empty)"
		}
		return escapeMarkdown(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "(unreadable)"
	}
	return escapeMarkdown(string(encoded))
}

func serverDescription(data map[string]any) string {
	parts := make([]string, 0, 3)
	if world := escapeMarkdown(stringField(data, "world")); world != "" {
		parts = append(parts, "world "+world)
	}
	if game := escapeMarkdown(stringField(data, "game")); game != "" {
		parts = append(parts, "game "+game)
	}
	if plugin, ok := data["plugin"].(map[string]any); ok {
		name := escapeMarkdown(stringField(plugin, "name"))
		version := escapeMarkdown(stringField(plugin, "version"))
		if name != "" {
			parts = append(parts, "plugin "+strings.TrimSpace(name+" "+version))
		}
	}
	if len(parts) == 0 {
		return "no details"
	}
	return strings.Join(parts, ", ")
}

func lastSeenDescription(data map[string]any) string {
	if lastSeen := stringField(data, "lastSeenAt"); lastSeen != "" {
		return "last traffic at " + escapeMarkdown(lastSeen)
	}
	return "no traffic recorded"
}

func actionIDField(data map[string]any) []discordField {
	actionID := stringField(data, "actionId")
	if actionID == "" {
		return nil
	}
	return []discordField{{Name: "Action", Value: clipText(escapeMarkdown(actionID), discordFieldValueMax), Inline: true}}
}

// playerLabel is the bold, escaped name of the player a payload names, the
// identity when there is no name, and "a player" when there is neither.
func playerLabel(data map[string]any) string {
	if name := escapeMarkdown(stringField(data, "name")); name != "" {
		return "**" + name + "**"
	}
	if label := identityLabel(data["player"]); label != "" {
		return "**" + label + "**"
	}
	return "a player"
}

// identityLabel renders a section 8.2 identity as platform:id, or "".
func identityLabel(value any) string {
	identity, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	id := stringField(identity, "id")
	if id == "" {
		return ""
	}
	if platform := stringField(identity, "platform"); platform != "" {
		return escapeMarkdown(platform + ":" + id)
	}
	return escapeMarkdown(id)
}

func stringField(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

func numberField(data map[string]any, key string) (float64, bool) {
	return numberValue(data[key])
}

// numberValue reads a decoded JSON number, whichever form the decoder gave it.
func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case float64:
		return typed, true
	}
	return 0, false
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(count) + " " + noun + "s"
}

// escapeMarkdown neutralizes Discord's formatting characters in text a
// player may have written, and replaces control characters other than a
// newline with a space so a name cannot carry a terminal escape into a log.
// A web address is broken with a zero-width space after its scheme, so
// Discord does not turn what a player typed into a live link; the text
// still reads as the address.
func escapeMarkdown(text string) string {
	text = urlScheme.ReplaceAllString(text, "${1}​//")
	text = urlBareHost.ReplaceAllString(text, "${1}​.")
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		switch r {
		case '\\', '*', '_', '~', '`', '|', '>', '#', '[', ']', '(', ')':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\n':
			out.WriteRune(r)
		default:
			if unicode.IsControl(r) {
				out.WriteByte(' ')
			} else {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// urlScheme and urlBareHost match what Discord would auto-link: a scheme
// name followed by "://", and the bare "www." form.
var (
	urlScheme   = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*:)//`)
	urlBareHost = regexp.MustCompile(`(?i)\b(www)\.`)
)

// clipText cuts text to limit runes, ending a cut text with an ellipsis.
func clipText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit <= 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// boundDiscordEmbed applies Discord's per-field and whole-embed limits.
func boundDiscordEmbed(embed discordEmbed) discordEmbed {
	embed.Title = clipText(embed.Title, discordTitleMax)
	embed.Description = clipText(embed.Description, discordDescriptionMax)
	embed.Footer.Text = clipText(embed.Footer.Text, discordFooterMax)
	if len(embed.Fields) > discordFieldsMax {
		embed.Fields = embed.Fields[:discordFieldsMax]
	}
	for i := range embed.Fields {
		embed.Fields[i].Name = clipText(embed.Fields[i].Name, discordFieldNameMax)
		embed.Fields[i].Value = clipText(embed.Fields[i].Value, discordFieldValueMax)
		if embed.Fields[i].Name == "" {
			embed.Fields[i].Name = "…"
		}
		if embed.Fields[i].Value == "" {
			embed.Fields[i].Value = "(empty)"
		}
	}
	// The whole-embed cap counts every text member; the description is what
	// gives when the sum is over, then the fields from the end.
	for embedTextLength(embed) > discordEmbedTotalMax {
		over := embedTextLength(embed) - discordEmbedTotalMax
		if length := len([]rune(embed.Description)); length > 1 {
			keep := length - over
			if keep < 1 {
				keep = 1
			}
			embed.Description = clipText(embed.Description, keep)
			continue
		}
		if len(embed.Fields) == 0 {
			break
		}
		embed.Fields = embed.Fields[:len(embed.Fields)-1]
	}
	return embed
}

func embedTextLength(embed discordEmbed) int {
	total := len([]rune(embed.Title)) + len([]rune(embed.Description)) + len([]rune(embed.Footer.Text))
	for _, field := range embed.Fields {
		total += len([]rune(field.Name)) + len([]rune(field.Value))
	}
	return total
}

// renderDeliveryBody renders one delivery's body for a webhook's template:
// the section 11.3 generic-json shape, or the discord embed.
func renderDeliveryBody(webhook store.Webhook, one notification, deliveryID string, serverName string) ([]byte, error) {
	if webhook.Template == templateDiscord {
		return renderDiscord(one, serverName)
	}
	data := one.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return json.Marshal(webhookPayload{
		DeliveryID: deliveryID,
		WebhookID:  webhook.ID,
		Type:       one.Type,
		ServerID:   one.ServerID,
		EventID:    one.EventID,
		OccurredAt: envelopeTimestamp(one.OccurredAt),
		Data:       data,
	})
}

// unknownTemplateMessage is the registration refusal for a template this hub
// does not implement (spec section 11.2).
func unknownTemplateMessage(template string) string {
	return fmt.Sprintf("template %s is not one this hub implements; %s and %s are", template, templateGenericJSON, templateDiscord)
}
