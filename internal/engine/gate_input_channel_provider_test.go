package engine

// gate_input_channel_provider_test.go — 结构守卫：引擎为门禁构造的每一份
// GateInput 都必须带上通道提供者。
//
// 为什么要钉这一条：命令门禁曾经"全绿但不可执行"，根因就在这一层——引擎构造
// GateInput 时从不填通道字段（`closure.go` 那几处只填 RunID/BatchID/TargetIDs），
// 而 verify 包自己的测试看不见这件事，因为它自己造 GateInput。#74 把接缝改成
// `ChannelFor`（按目标拨号）之后，"某一处忘了填"就是同族缺陷复发的方式：那个位置
// 上的门禁会退回失败关闭，或者更糟——用一台机器的结果替整批签字。
//
// 同一条守卫还反向钉一件事：引擎**不许**直接填 `Channel:`。单通道形态等于把"这一批
// 里随便挑一台来证明全部"写进结构，正是当初要修的语义。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 引擎今天构造 GateInput 的位置数（closure.go 四处）。低于这个数说明有构造点
// 消失了或这个测试的标记串失效——两种情况都必须红，"0 命中"从来不是通过。
const minEngineGateInputSites = 4

func TestEveryEngineGateInputCarriesChannelProvider(t *testing.T) {
	var offenders []string
	total := 0

	require.NoError(t, filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(data)
		for _, lit := range compositeLiterals(text, "verify.GateInput{") {
			total++
			body := lit.body
			if !strings.Contains(body, "ChannelFor:") {
				offenders = append(offenders, path+": GateInput without ChannelFor")
			}
			if strings.Contains(body, "Channel:") {
				// `ChannelFor:` 不含子串 "Channel:"，所以这里只命中真正的单通道字段。
				offenders = append(offenders, path+": GateInput pre-dials a single Channel")
			}
		}
		return nil
	}))

	assert.GreaterOrEqual(t, total, minEngineGateInputSites,
		"only %d GateInput literals found — the marker or a wiring site disappeared; a vacuous scan is not a pass", total)
	assert.Empty(t, offenders, "engine must supply the channel provider at every gate call site: %v", offenders)
}

type gateLiteral struct {
	body string
}

// compositeLiterals returns each `marker` composite literal together with the
// text between its braces, found by brace counting rather than a regex so a
// nested literal or a `}}` inside a string still terminates in the right place
// for the purpose of this scan (the fields we look for are always top level).
func compositeLiterals(text, marker string) []gateLiteral {
	var out []gateLiteral
	for i := 0; ; {
		idx := strings.Index(text[i:], marker)
		if idx < 0 {
			return out
		}
		start := i + idx + len(marker) // just after '{'
		depth := 1
		for j := start; j < len(text) && depth > 0; j++ {
			switch text[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					out = append(out, gateLiteral{body: text[start:j]})
				}
			}
		}
		i = start
	}
}
