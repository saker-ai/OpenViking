// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

// TrajectoryMemoryType is the memory type for agent execution trajectories.
const TrajectoryMemoryType = "trajectories"

// ExperienceMemoryType is the memory type for consolidated agent experiences.
const ExperienceMemoryType = "experiences"

// ExecutionMemoryTypes lists memory types produced by agent execution
// (trajectories + experiences). It mirrors
// openviking.session.memory.constants.EXECUTION_MEMORY_TYPES.
var ExecutionMemoryTypes = map[string]struct{}{
	TrajectoryMemoryType: {},
	ExperienceMemoryType: {},
}

// IsExecutionMemoryType reports whether mt is one of the agent-execution
// memory types (trajectories or experiences).
func IsExecutionMemoryType(mt string) bool {
	_, ok := ExecutionMemoryTypes[mt]
	return ok
}
