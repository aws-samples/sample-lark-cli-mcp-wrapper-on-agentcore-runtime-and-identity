package main

import (
	"context"
	"fmt"
)

// runLarkListSkills lists all available lark-cli Skills.
func runLarkListSkills(ctx context.Context) (execResult, error) {
	return runLarkExec(ctx, "skills list")
}

// runLarkGetSkill fetches the detailed orchestration guide for one Skill by name.
func runLarkGetSkill(ctx context.Context, skillName string) (execResult, error) {
	if skillName == "" {
		return execResult{}, fmt.Errorf("skillName is required")
	}
	return runLarkExec(ctx, fmt.Sprintf("skills read %s", skillName))
}
