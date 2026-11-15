package modules

import (
	"strconv"
	"strings"

	"mimamori/app/models"
)

// CLIVars returns the variables of the command line definitions (${name} in
// the tags of the command structs, given to the parser as kong.Vars): values
// the program defines in one place, and the accepted values and help texts
// of the flags made from them.
//
//   - default_agent_port: DefaultAgentPort.
//   - detection_categories, detection_types, history_categories: the
//     accepted values of a filter flag (DetectionCategories, DetectionTypes
//     or models.HistoryCategories), the enum of the flag ("a,b,c").
//   - the same names with _help: those values for the text of --help ("a, b
//     or c"), and with _placeholder: for the placeholder ("a|b|c").
func CLIVars() map[string]string {
	vars := map[string]string{"default_agent_port": strconv.Itoa(DefaultAgentPort)}
	for name, list := range map[string][]string{
		"detection_categories": DetectionCategories,
		"detection_types":      DetectionTypes,
		"history_categories":   models.HistoryCategories,
	} {
		vars[name] = strings.Join(list, ",")
		vars[name+"_help"] = orList(list)
		vars[name+"_placeholder"] = strings.Join(list, "|")
	}
	return vars
}

// orList joins values as "a, b or c" (a single value alone).
func orList(values []string) string {
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}
