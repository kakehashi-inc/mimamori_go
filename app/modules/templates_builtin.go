package modules

import "sort"

// BuiltinTemplate is a template shipped with the application. It cannot be
// changed or deleted.
type BuiltinTemplate struct {
	Name        string
	Description string
	Content     RuleContent
}

func ign(values ...string) []Element {
	out := make([]Element, 0, len(values))
	for _, v := range values {
		out = append(out, Element{Value: v, IfMissing: IfMissingIgnore})
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// builtinTemplates lists the built-in templates. Names and paths that differ
// by OS are both listed as "ignore if missing" elements.
var builtinTemplates = []BuiltinTemplate{
	{
		Name:        "base",
		Description: "Common categories and intervals, read monitoring of /etc/ssh and /home, scan origin /opt and executable file extensions",
		Content: RuleContent{
			ExcludeInteractive: boolPtr(true),
			CheckIntervalSec:   DefaultCheckIntervalSec,
			ScanIntervalSec:    DefaultScanIntervalSec,
			Exec:               CategoryOnly{Category: CategoryAlert},
			Denied:             CategoryOnly{Category: CategoryNormal},
			Read:               ReadSpec{Dirs: ign("/etc/ssh", "/home"), Category: CategoryAlert},
			Network:            NetworkSpec{Category: CategoryAlert},
			Writable:           WritableSpec{Base: DefaultScanBase, Category: CategoryAlert},
			ExecFiles: ExecFilesSpec{
				Extensions:    []string{".php", ".phar", ".phtml", ".js", ".mjs", ".html", ".htm", ".svg", ".xhtml"},
				OtherCategory: CategoryAlert,
			},
		},
	},
	{
		Name:        "apache",
		Description: "Apache HTTP Server users (apache, www-data) and read monitoring of its configuration",
		Content: RuleContent{
			Users: ign("apache", "www-data"),
			Read:  ReadSpec{Dirs: ign("/etc/httpd", "/etc/apache2")},
		},
	},
	{
		Name:        "nginx",
		Description: "nginx users (nginx, www-data) and read monitoring of its configuration",
		Content: RuleContent{
			Users: ign("nginx", "www-data"),
			Read:  ReadSpec{Dirs: ign("/etc/nginx")},
		},
	},
	{
		Name:        "php-fpm",
		Description: "Read monitoring of the PHP-FPM configuration. Add the pool users of each site to the expanded rule",
		Content: RuleContent{
			Read: ReadSpec{Dirs: ign("/etc/php-fpm.d", "/etc/php", "/etc/opt/remi")},
		},
	},
	{
		Name:        "laravel",
		Description: "Laravel writable paths (storage, bootstrap/cache) and the places where PHP files are created legitimately",
		Content: RuleContent{
			Writable: WritableSpec{AllowedPaths: []string{"storage", "bootstrap/cache"}},
			ExecFiles: ExecFilesSpec{
				Legit: []LegitLocation{
					{Path: "bootstrap/cache", Extensions: []string{".php"}, Category: CategoryNormal},
					{Path: "storage/framework/views", Extensions: []string{".php"}, Category: CategoryNormal},
				},
			},
		},
	},
	{
		Name:        "ffmpeg",
		Description: "Network monitoring of ffmpeg and ffprobe under /opt/ffmpeg",
		Content: RuleContent{
			Network: NetworkSpec{Executables: ign("/opt/ffmpeg/bin/ffmpeg", "/opt/ffmpeg/bin/ffprobe")},
		},
	},
}

// BuiltinTemplates returns copies of the built-in templates, sorted by name.
func BuiltinTemplates() []BuiltinTemplate {
	out := make([]BuiltinTemplate, 0, len(builtinTemplates))
	for _, t := range builtinTemplates {
		c := t.Content.Clone()
		out = append(out, BuiltinTemplate{Name: t.Name, Description: t.Description, Content: *c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindBuiltinTemplate returns a copy of the named built-in template.
func FindBuiltinTemplate(name string) (*BuiltinTemplate, bool) {
	for _, t := range BuiltinTemplates() {
		if t.Name == name {
			return &t, true
		}
	}
	return nil, false
}
