package code

import (
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/saker-ai/ctxhub/internal/parse"
)

func init() {
	(&CodeParser{
		Name: "javascript",
		Ext:  ".js",
		Lang: javascript.GetLanguage(),
	}).register()
	parse.RegisterExtension(".mjs", "javascript")
	parse.RegisterExtension(".cjs", "javascript")
}
