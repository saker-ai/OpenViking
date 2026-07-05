// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

// Package memory ports the openviking.session.memory Python subsystem to
// Go. It provides the memory type registry, schema model generator, merge
// operations (SEARCH/REPLACE patch handler), memory file I/O, context
// providers, and the streaming memory updater that the bot agent uses to
// extract, merge, and persist session-derived memories (skills, entities,
// facts).
//
// The package is organised in tiers:
//
//   - Tier 1 (data types + registry): constants.go, dataclass.go,
//     registry.go, schema.go
//   - Tier 2 (merge ops + utils): merge_op.go, patch_handler.go,
//     utils.go, tools.go, page_id_map.go, vision_normalizer.go
//   - Tier 3 (context providers): experience_provider.go,
//     trajectory_provider.go, patch_merge_provider.go,
//     session_extract_provider.go
//   - Tier 4 (updaters): core.go, extract_loop.go, updater.go,
//     streaming_updater.go, isolation_handler.go, graph_view.go
//
// Persistence goes through internal/ragfs.FileSystem (the same interface
// used by internal/session/toolresult/store.go) and modernc.org/sqlite
// for any structured storage (e.g. the page_id_map).
package memory
