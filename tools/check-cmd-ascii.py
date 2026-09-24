#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""start.cmd 批处理健壮性自查（改 start.cmd 后跑一下）。

为什么需要：cmd.exe 逐行重读批处理文件并按控制台代码页解释字节，本机 chcp=936 而文件是
UTF-8，会出三类静默故障（详见 HANDOFF §14 第 16 条）：

  1. GBK 解码把汉字尾字节与紧随其后的 ASCII 字符配成双字节 → 吞掉字符（"restart" → "art"）；
  2. rem/echo 行里的 | < > & ^ 被当成管道/重定向先解析；
  3. chcp 65001 之后再出现非 ASCII 字节 → 已记录的字节偏移错位 → 后续行解析成乱码。

用法: python tools/check-cmd-ascii.py [start.cmd panel.cmd ...]   # 默认查全部 .cmd
退出码: 0 通过 / 1 有问题（便于接进守卫或 CI）。
"""
import sys
import os

METACHARS = "|<>&^"


def check(path):
    if not os.path.isfile(path):
        return ["文件不存在: %s" % path]
    raw = open(path, "rb").read()
    lines = raw.decode("utf-8", "replace").split("\n")
    problems = []

    # 1. 全文非 ASCII 字节（.cmd 必须纯 ASCII，中文只放 .sh）
    n_na = sum(1 for b in raw if b > 127)
    if n_na:
        first = next(i for i, l in enumerate(lines, 1) if any(ord(c) > 127 for c in l))
        problems.append(
            "含 %d 个非 ASCII 字节（首个在第 %d 行）：.cmd 须纯 ASCII，中文说明放 start.sh"
            % (n_na, first))

    # 2. rem / echo 行里的元字符
    for i, l in enumerate(lines, 1):
        s = l.strip().lower()
        if s.startswith("rem") or s.startswith("::"):
            for ch in METACHARS:
                if ch in l:
                    problems.append("第 %d 行 rem 含元字符 %r（会被当管道/重定向解析）：%s"
                                    % (i, ch, l.strip()[:60]))
                    break

    # 3. chcp 65001 之后不得再有非 ASCII
    ci = None
    for i, l in enumerate(lines):
        if l.lower().startswith("chcp") and "65001" in l:
            ci = i
            break
    if ci is not None:
        after = [(i + 1, l) for i, l in enumerate(lines[ci + 1:], ci + 1)
                 if any(ord(c) > 127 for c in l)]
        if after:
            problems.append("chcp 65001 之后仍有非 ASCII：第 %s 行（偏移错位会让后续行解析成乱码）"
                            % ", ".join(str(i) for i, _ in after[:5]))
    return problems


def main():
    # 默认查仓库根下所有 .cmd（新加入口别漏检）
    targets = sys.argv[1:] or sorted(f for f in os.listdir(".") if f.lower().endswith(".cmd"))
    bad = 0
    for t in targets:
        problems = check(t)
        if problems:
            bad = 1
            print("[FAIL] %s" % t)
            for p in problems:
                print("  - %s" % p)
        else:
            print("[ok] %s：纯 ASCII，rem 无元字符，chcp 之后无非 ASCII" % t)
    return bad


if __name__ == "__main__":
    sys.exit(main())
