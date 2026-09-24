#!/usr/bin/env python3
"""从官方客户端日志中抽取**真实请求体**的顶层键（SPEC §33.1 审计第 1 步的取证工具）。

用途：官方客户端在请求失败时会连同 body 一起打进日志（如 429 记录）。本工具在日志里
定位这些请求体、做括号配平解析，并列出顶层键与关键取值——用来确定"官方客户端到底发什么"，
据此约束我们**发送侧**的形态（发送形态即指纹，见 SPEC §33.4）。

用法：
    python tools/extract-client-request.py "<客户端日志路径>" [--show messages,tools]

默认只打印键与类型（避免把整段 messages 刷屏）。
"""
import argparse
import json
import sys

BODY_KEYS = ('"body":', '"requestBody":', '{"model"')
INTERESTING = ("model", "stream", "temperature", "top_p", "max_tokens",
               "max_output_tokens", "tool_choice", "reasoning_effort", "stop",
               "seed", "frequency_penalty", "presence_penalty", "logprobs")


def balanced_json(text, start):
    """从 text[start]（'{'）起做括号配平提取完整 JSON（考虑字符串与转义）。"""
    depth, instr, esc = 0, False, False
    for i in range(start, len(text)):
        ch = text[i]
        if instr:
            if esc:
                esc = False
            elif ch == "\\":
                esc = True
            elif ch == '"':
                instr = False
            continue
        if ch == '"':
            instr = True
        elif ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                return text[start:i + 1]
    return None


def extract(path, anchor, show):
    raw = open(path, "rb").read().decode("utf-8", "replace")
    idx = raw.find(anchor)
    if idx < 0:
        print("未找到锚点 %s" % anchor)
        return 1
    window = raw[max(0, idx - 400000):idx + 400]
    for key in BODY_KEYS:
        j = window.rfind(key)
        if j < 0:
            continue
        start = window.find("{", j)
        body = balanced_json(window, start)
        if not body:
            continue
        try:
            obj = json.loads(body)
        except Exception as e:  # noqa: BLE001
            print("锚点 %s 解析失败：%s" % (key, e))
            continue
        print("=== 官方请求体顶层键（锚点 %s）===" % key)
        for k, v in obj.items():
            note = ""
            if k in INTERESTING:
                note = " = %r" % (v,)
            elif isinstance(v, list):
                note = " (n=%d)" % len(v)
            if k in show:
                note = " = " + json.dumps(v, ensure_ascii=False)[:400]
            print("   %-22s %-8s%s" % (k, type(v).__name__, note))
        return 0
    print("窗口内未找到可解析的请求体（锚点 %s）" % anchor)
    return 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("logfile")
    ap.add_argument("--anchor", default='"statusCode":429',
                    help='定位日志行的锚点（默认取一条失败请求，它连 body 一起打）')
    ap.add_argument("--show", default="",
                    help="额外完整打印的键，逗号分隔（如 messages,tools）")
    args = ap.parse_args()
    show = {s.strip() for s in args.show.split(",") if s.strip()}
    return extract(args.logfile, args.anchor, show)


if __name__ == "__main__":
    sys.exit(main())
