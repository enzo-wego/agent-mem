package pluginassets

import "embed"

// SkillFS contains the bundled plugin skill directories shipped with agent-mem.
//
//go:embed all:skills
var SkillFS embed.FS
