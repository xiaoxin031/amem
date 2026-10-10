<div align="center">

# amem

**给 AI 编程助手用的轻量、自动记忆。**
Claude Code、Codex 和任何 MCP 客户端共用同一份记忆。开会话时按固定大小加载，改完代码自动保存。

[![ci](https://github.com/xiaoxin031/amem/actions/workflows/ci.yml/badge.svg)](https://github.com/xiaoxin031/amem/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/xiaoxin031/amem)](https://github.com/xiaoxin031/amem/releases)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

[English](README.md) · 中文

</div>

```sh
curl -fsSL https://raw.githubusercontent.com/xiaoxin031/amem/main/install.sh | sh
```

装完就能用。下一次开 Claude Code 或 Codex，它已经知道你是谁、这个项目是怎么回事，之后边干活边自己更新这些记忆。

---

## 为什么要用

AI 编程助手每开一个新会话就什么都不记得了。常见的几种补救办法各有问题：

| | 一直追加的 `memories.md` / `/save` | 厂商自带记忆 | **amem** |
|---|---|---|---|
| 每次开会话的上下文 | 越积越多（实测某项目已达 **117 KB**） | 看不到 | **固定约 5 KB** |
| 怎么保存 | 手动敲 `/save` | 自动，但只限自家工具 | **改完代码自动保存** |
| 多个工具之间共享 | 不能 | 不能 | **Claude Code、Codex、所有 MCP 客户端** |
| 过时的信息 | 越堆越多 | 不清楚 | **被替换或合并，有硬性上限** |
| 数据在哪 | markdown 文件 | 厂商的云端 | **`~/.amem` 里的纯文本** |

amem 的做法是：常驻上下文的只有一小份精炼过的记忆，其余内容都放进归档，需要的时候再去搜。

## 有什么好处

- **上下文大小固定**：开会话时只注入你的画像、全局笔记、本项目笔记，加上本项目最近 3 次会话的摘要。每个记忆库都有字数上限，所以不管历史积累多少，总量始终在 5 KB 左右。
- **不用手动 `/save`**：AI 改了文件之后会被提醒记下要点。改文件的方式不限：编辑工具、打补丁、`sed`、重定向都算，靠对比 git 状态来判断。没改文件时每 8 轮提醒一次。保存就在当前会话里完成，能复用 prompt cache，额外成本只是一小段回复。
- **越用越精，不会越用越大**：写入是原子操作，一批 增/改/删 要么全部成功要么全部不改。写满了就整批拒绝，并把现有条目返回给 AI，让它先合并或删掉过时的内容。
- **一份记忆，所有工具共用**：Claude 学到的东西，下次开 Codex 也知道。
- **需要时再查**：历史会话摘要和长文档都能搜（`amem search` 命令或 `memory_search` 工具）。少见的词排序权重更高。用的是子串匹配，中文、日文、韩文不需要分词。
- **零配置**：安装器会自动注册 hook，在 Codex 里直接设为已信任；注册 MCP 工具并设为自动放行，所以写记忆不会被沙箱或权限弹窗拦住。改动的每个文件都会先备份。
- **小而稳**：单个 Go 二进制，约 3 MB。不需要运行时、数据库、API key，也不联网。生成一次快照约 12 毫秒。支持 macOS、Linux、Windows，x64 和 ARM 都有。

## 工作原理

```
                开会话                                  改完代码那一轮结束
                  │                                          │
   ┌──────────────▼───────────────────┐       ┌──────────────▼──────────────┐
   │ SessionStart hook → 注入快照      │       │ Stop hook → "把要点记下来"   │
   │  USER.md     你是谁       ≤1200   │       │  memory_save  长期事实        │
   │  MEMORY.md   跨项目事实   ≤2500   │       │  memory_log   每个会话一条摘要 │
   │  项目记忆    本项目约定   ≤2500   │       └──────────────┬──────────────┘
   │  + 最近 3 次会话摘要             │                      ▼
   └──────────────────────────────────┘          ~/.amem（纯文本，归你所有）
         memory_search / amem search  ◀──────  history.jsonl · notes/*.md
```

设计参考了 [hermes-agent](https://github.com/NousResearch/hermes-agent) 的记忆系统：

- 每个记忆库有上限
- 快照在开会话时固定下来，中途不变，prompt cache 一直有效
- 归档不进 prompt

## 安装

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/xiaoxin031/amem/main/install.sh | sh
```

**Windows（PowerShell）**

```powershell
irm https://raw.githubusercontent.com/xiaoxin031/amem/main/install.ps1 | iex
```

**从源码装**（装了 Go 就本地编译，没装就下载已发布的二进制）：

```sh
git clone https://github.com/xiaoxin031/amem && cd amem && ./install.sh
```

安装脚本会把 `amem` 放到 `~/.local/bin`，然后执行 `amem install`。它会自动识别本机装了 Claude Code 还是 Codex，并接好：

| | Claude Code | Codex |
|---|---|---|
| 开会话注入快照 | `SessionStart` hook | `SessionStart` hook，直接设为已信任 |
| 自动保存 | `Stop` hook | `Stop` hook，直接设为已信任 |
| 记忆工具 | MCP server `amem`，已加入白名单 | MCP server `amem`，自动放行 |
| 手动保存 | `/save` | `$save` |

改动任何文件前都会先备份成 `.bak-amem-时间戳`。后来才装 Claude 或 Codex 的话，再执行一次 `amem install` 就行，重复执行也不会出问题。

**其他工具（Cursor、Cline、Gemini CLI、opencode 等）** 加一个 MCP server 即可：

```json
{ "mcpServers": { "amem": { "command": "amem", "args": ["mcp"] } } }
```

这类工具没有生命周期 hook，不能自动保存。在它的规则文件里加一句："开始时调用 memory_load；改完代码后调用 memory_save 和 memory_log。"

## 日常用法

平时什么都不用做。想看看记了什么，可以用这些命令：

```sh
amem ls                      # 当前记忆，以及每个库用了多少
amem search 部署 staging     # 搜历史会话和笔记
amem inject                  # AI 开会话时实际看到的内容
amem add user "回答要简短"
amem rm project "旧的构建命令"
amem import ./old-notes/     # 把已有的 markdown 导入成可搜索的笔记
```

## 配置（可选）

配置文件是 `~/.amem/config.json`：

```json
{ "limits": { "user": 1200, "memory": 2500, "project": 2500 }, "review_every": 8 }
```

- `limits`：每个记忆库的字数上限，快照能保持小就靠它
- `review_every`：没改文件时，每隔多少轮自动保存一次。设为 `0` 表示只在改文件后保存，设为 `-1` 表示关闭自动保存
- 环境变量：`AMEM_HOME` 可以改数据目录的位置；`CLAUDE_CONFIG_DIR` 和 `CODEX_HOME` 也会被识别

## 常见问题

**自动保存费 token 吗？** 不费。它只是同一个会话里多一段回复，走 prompt cache。而且只在改过文件之后，或者每 8 轮，才触发一次。

**数据会上传吗？** 不会。amem 不联网，记忆就是 `~/.amem` 里的纯文本，你可以直接查看、修改，也可以放进 git 管理。

**怎么卸载？** 执行 `amem uninstall`，会移除 hook、MCP server、权限白名单和 `/save`。记忆数据会保留，不想要了自己删掉 `~/.amem` 就行。

## 许可证

[MIT](LICENSE)
