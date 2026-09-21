package aichat

// artifact_reference.go 是 internal/handler/session/artifact_reference.go 的
// 私有镜像；本文件固化 v1 协议路径依赖的关键行为（sandbox: 前缀、裸文件名
// 图片、候选顺序），防止两份实现静默漂移。

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// handleFor builds a syntactically valid resource handle for artifact i so the
// rewrite exercises the same ParseResourcePath path as production.
func handleFor(i int) string {
	base := fmt.Sprintf("art%d", i)
	return base + strings.Repeat("x", types.ResourceHandleLength-len(base))
}

func refFor(i int) string {
	return types.BuildResourcePath(handleFor(i))
}

func artifactsFixture(names ...string) types.MessageArtifacts {
	list := make(types.MessageArtifacts, 0, len(names))
	for i, name := range names {
		list = append(list, types.MessageArtifact{FileName: name, URL: refFor(i)})
	}
	return list
}

func TestRewriteArtifactReferencesSandboxSpellings(t *testing.T) {
	artifacts := artifactsFixture("task_distribution_chart.png", "task_distribution.csv")

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "sandbox prefix",
			content: "![图表](sandbox:task_distribution_chart.png)",
			want:    "![图表](" + refFor(0) + ")",
		},
		{
			name:    "bare name in image",
			content: "![图表](task_distribution_chart.png)",
			want:    "![图表](" + refFor(0) + ")",
		},
		{
			name:    "output directory prefix",
			content: "[明细](/workspace/output/task_distribution.csv)",
			want:    "[明细](" + refFor(1) + ")",
		},
		{
			name:    "percent-encoded name",
			content: "![图表](sandbox:task_distribution_chart.png)",
			want:    "![图表](" + refFor(0) + ")",
		},
		{
			name:    "http url untouched",
			content: "![远程](https://example.com/chart.png)",
			want:    "![远程](https://example.com/chart.png)",
		},
		{
			name:    "unknown name untouched",
			content: "![别的](missing.png)",
			want:    "![别的](missing.png)",
		},
		{
			name:    "code fence untouched",
			content: "```\n![图表](task_distribution_chart.png)\n```",
			want:    "```\n![图表](task_distribution_chart.png)\n```",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, rewriteArtifactReferences(tc.content, artifacts))
		})
	}
}

// 无资源目录的部署降级为 sandbox:<name> 规范拼写，且不泄漏存储路径。
func TestRewriteArtifactReferencesWithoutCatalog(t *testing.T) {
	artifacts := types.MessageArtifacts{
		{FileName: "chart.png", URL: "local://7/exports/chart.png"},
	}
	got := rewriteArtifactReferences("![图表](chart.png)", artifacts)
	require.Equal(t, "![图表](sandbox:chart.png)", got)
	require.NotContains(t, got, "local://")
}

// 本轮产物优先于会话内旧版本：同名文件解析到第一个候选；显式句柄引用
// 仍能指到旧版本，且旧版本被识别为需要扩展归属的历史产物。
func TestReferencedArtifactsPrefersThisTurn(t *testing.T) {
	old := types.MessageArtifact{FileName: "deck.pptx", URL: refFor(0)}
	fresh := types.MessageArtifact{FileName: "deck.pptx", URL: refFor(1)}
	known := types.MessageArtifacts{old}
	candidates := mergeArtifactLists(types.MessageArtifacts{fresh}, artifactsNewestFirst(known))

	require.Equal(t, types.MessageArtifacts{fresh},
		referencedArtifacts("![deck](sandbox:deck.pptx)", candidates))
	require.Equal(t, types.MessageArtifacts{old},
		referencedArtifacts("![deck]("+old.URL+")", candidates))
	require.Equal(t, types.MessageArtifacts{old},
		historyOnlyArtifacts(
			referencedArtifacts("![deck]("+old.URL+")", candidates),
			types.MessageArtifacts{fresh}))
}

// 公开视图只暴露元数据与句柄，绝不包含存储 URL。
func TestPublicArtifactViewsRedactsStoragePath(t *testing.T) {
	views := publicArtifactViews(artifactsFixture("chart.png"))
	require.Len(t, views, 1)
	require.Equal(t, 0, views[0]["index"])
	require.Equal(t, "chart.png", views[0]["file_name"])
	require.Equal(t, refFor(0), views[0]["handle"])
	require.NotContains(t, views[0], "url")
}
