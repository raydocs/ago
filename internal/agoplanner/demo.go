package agoplanner

import (
	"context"
	"fmt"
)

// DemoPlanner turns any Chinese objective into the deterministic offline
// fixture DAG described by the usable demo delivery plan. It performs no model
// call, so the demo path works without network access or provider credentials.
//
// The objective and repository are carried through unchanged; only the task
// decomposition is synthesized. The planner fails closed when the caller's
// constraints cannot host the tasks it needs to emit, so an unsatisfiable
// request never reaches a work graph.
type DemoPlanner struct{}

const (
	demoScopeReadme = "README.md"
	demoScopeDocs   = "docs"

	demoCapabilityRead   = "repo-read"
	demoCapabilityWrite  = "repo-write"
	demoCapabilityTests  = "tests"
	demoCapabilityReport = "report"
)

type demoTask struct {
	id          string
	title       string
	description string
	scopes      []string
	capability  string
	criteria    []string
	dependsOn   []string
}

// demoTasks is the fixed offline decomposition. Read and command discovery are
// independent roots so the future scheduler has genuinely parallel work.
var demoTasks = []demoTask{
	{
		id: "inspect-repository", title: "检查仓库结构与元数据",
		description: "读取仓库的目录结构、说明文档与项目元数据，记录快速开始章节需要引用的事实。",
		capability:  demoCapabilityRead,
		criteria:    []string{"记录仓库结构与关键文件", "列出快速开始章节需要的事实"},
	},
	{
		id: "identify-commands", title: "识别构建与测试命令",
		description: "从仓库配置中识别可复现的构建与测试命令，并记录它们的执行前提。",
		capability:  demoCapabilityRead,
		criteria:    []string{"记录可执行的构建与测试命令"},
	},
	{
		id: "update-readme", title: "更新 README 快速开始章节",
		description: "根据仓库检查结果，为 README 增加一个可直接执行的快速开始章节。",
		scopes:      []string{demoScopeReadme}, capability: demoCapabilityWrite,
		criteria:  []string{"README 包含快速开始章节", "章节中的命令与仓库实际配置一致"},
		dependsOn: []string{"inspect-repository", "identify-commands"},
	},
	{
		id: "run-tests", title: "运行相关测试并记录结果",
		description: "执行已识别的测试命令，记录退出码、耗时与输出引用作为验收证据。",
		capability:  demoCapabilityTests,
		criteria:    []string{"测试命令返回确定性结果", "记录退出码与输出引用"},
		dependsOn:   []string{"identify-commands"},
	},
	{
		id: "write-report", title: "生成完成报告",
		description: "汇总仓库分析、文档变更与测试证据，生成可供人工复核的完成报告。",
		scopes:      []string{demoScopeDocs}, capability: demoCapabilityReport,
		criteria:  []string{"报告引用测试证据与变更文件", "报告说明未解决的风险"},
		dependsOn: []string{"update-readme", "run-tests"},
	},
}

func (DemoPlanner) Plan(ctx context.Context, request Request) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	if err := request.validate(); err != nil {
		return Plan{}, fmt.Errorf("invalid planner request: %w", err)
	}
	for _, scope := range []string{demoScopeReadme, demoScopeDocs} {
		if !scopeAllowed(scope, request.Constraints.PathScopes) {
			return Plan{}, fmt.Errorf("demo plan requires allowed path scope %q", scope)
		}
	}
	for _, capability := range []string{demoCapabilityRead, demoCapabilityWrite, demoCapabilityTests, demoCapabilityReport} {
		if err := valuesAllowed("capability", []string{capability}, request.Constraints.CapabilityTags); err != nil {
			return Plan{}, fmt.Errorf("demo plan: %w", err)
		}
	}
	verifier := request.Constraints.VerifierIDs[0]

	plan := Plan{
		SchemaVersion: SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		Tasks:         make([]TaskProposal, 0, len(demoTasks)),
		Dependencies:  []DependencyProposal{},
		ProjectGates:  clonePlan(Plan{ProjectGates: request.ProjectGates}).ProjectGates,
	}
	for _, task := range demoTasks {
		scopes := task.scopes
		if len(scopes) == 0 {
			scopes = request.Constraints.PathScopes
		}
		plan.Tasks = append(plan.Tasks, TaskProposal{
			ID: task.id, Title: task.title, Description: task.description,
			PathScopes:         append([]string(nil), scopes...),
			AcceptanceCriteria: append([]string(nil), task.criteria...),
			VerifierIDs:        []string{verifier},
			CapabilityTags:     []string{task.capability},
		})
		for _, prerequisite := range task.dependsOn {
			plan.Dependencies = append(plan.Dependencies, DependencyProposal{TaskID: task.id, DependsOn: prerequisite})
		}
	}
	if err := plan.Validate(request); err != nil {
		return Plan{}, fmt.Errorf("demo plan is not admissible: %w", err)
	}
	return plan, nil
}
