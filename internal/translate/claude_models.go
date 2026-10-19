package translate

import (
	"regexp"
	"strconv"
)

// Claude request-surface facts the Messages encoder needs. Current models
// reject parameters older ones accept, so a translated body must follow the
// model it is going to. The id is matched wherever a provider puts it
// (anthropic/claude-sonnet-5.5, us.anthropic.claude-opus-4-8,
// claude-haiku-5-5[1m]); an id that is not a recognizable Claude model gets
// no adjustment at all.

var claudeModelID = regexp.MustCompile(`claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:[-.](\d{1,2}))?(?:[^0-9]|$)`)

type claudeModel struct {
	family       string
	major, minor int
}

func parseClaudeModel(id string) (claudeModel, bool) {
	m := claudeModelID.FindStringSubmatch(id)
	if m == nil {
		return claudeModel{}, false
	}
	c := claudeModel{family: m[1]}
	c.major, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		c.minor, _ = strconv.Atoi(m[3])
	}
	return c, true
}

func (c claudeModel) atLeast(major, minor int) bool {
	return c.major > major || c.major == major && c.minor >= minor
}

// frontier reports the Fable and Mythos tiers, whose surface leads the rest.
func (c claudeModel) frontier() bool { return c.family == "fable" || c.family == "mythos" }

// rejectsSampling: temperature and top_p are a 400 (or accept only their
// defaults) on Opus 4.7 and later, every 5.x model, Fable and Mythos.
func (c claudeModel) rejectsSampling() bool {
	if c.frontier() {
		return true
	}
	if c.family == "opus" && c.atLeast(4, 7) {
		return true
	}
	return c.major >= 5
}

// rejectsForcedTools: tool_choice any/tool is a 400 on Fable 5.1, Mythos 5.1,
// Opus 5.5 and Sonnet 5.5 (and, by the same line, their successors).
func (c claudeModel) rejectsForcedTools() bool {
	switch c.family {
	case "fable", "mythos":
		return c.atLeast(5, 1)
	case "opus", "sonnet":
		return c.atLeast(5, 5)
	}
	return false
}

// thinksByDefault: the model runs adaptive thinking when the request names
// none, so asking for a summarized display changes visibility only. On
// Opus 4.7/4.8 and the 4.6 models a thinking parameter would turn it on.
func (c claudeModel) thinksByDefault() bool {
	return c.frontier() || c.major >= 5
}
