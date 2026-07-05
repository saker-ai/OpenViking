package code

import (
	"github.com/smacker/go-tree-sitter/lua"
)

func init() {
	(&CodeParser{
		Name: "lua",
		Ext:  ".lua",
		Lang: lua.GetLanguage(),
	}).register()
}
