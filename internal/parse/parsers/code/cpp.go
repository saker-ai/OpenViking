package code

import (
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/saker-ai/ctxhub/internal/parse"
)

func init() {
	(&CodeParser{
		Name: "cpp",
		Ext:  ".cpp",
		Lang: cpp.GetLanguage(),
	}).register()
	parse.RegisterExtension(".cc", "cpp")
	parse.RegisterExtension(".cxx", "cpp")
	parse.RegisterExtension(".hpp", "cpp")
	parse.RegisterExtension(".hh", "cpp")
	parse.RegisterExtension(".h", "cpp")
}
