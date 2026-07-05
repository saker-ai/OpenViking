package code

import (
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/saker-ai/ctxhub/internal/parse"
)

func init() {
	(&CodeParser{
		Name: "typescript",
		Ext:  ".ts",
		Lang: typescript.GetLanguage(),
	}).register()
	(&CodeParser{
		Name: "tsx",
		Ext:  ".tsx",
		Lang: tsx.GetLanguage(),
	}).register()
	parse.RegisterExtension(".cts", "typescript")
	parse.RegisterExtension(".mts", "typescript")
}
