#!/usr/bin/env python3
"""变异测试：一次破坏一个护栏，确认对应测试变红，然后恢复。

为什么要有这个脚本：测试全绿只证明"代码通过了断言"，不证明"断言真的
在守着什么"。把护栏逐条破坏掉，如果测试还绿，那条断言就是空转的。

安全性：进程开始时把所有涉及文件快照到内存，atexit 无条件写回。
上一版在测试后抛了异常，变异没恢复，导致 store.go 带着被改坏的 SQL
继续存在了一整轮开发 —— 别再依赖"记得手动恢复"。

用法：python3 tools/service-store-mutate.py
退出码 0 = 全部变异被抓到；1 = 有变异存活（需补测试或确认为等价变异）。
"""
import atexit
import shutil
import subprocess
import sys
import tempfile

PKG = "./internal/service/"
STORE = "internal/service/store.go"

MUTATIONS = [
    (
        "H: 重置范围扩大到 stopped（会抹掉历史退出信息）",
        STORE,
        "\t\tWHERE state IN (?, ?)`,\n\t\tStateStopped, StateRunning, StateStarting)",
        "\t\tWHERE state IN (?, ?, ?)`,\n\t\tStateStopped, StateRunning, StateStarting, StateStopped)",
        "TestResetStatesOnBoot",
    ),
    (
        "I: 重置只清 state，留着 pid/pgid（重启后 UI 显示已死进程的 PID）",
        STORE,
        "state=?, pid=0, pgid=0, started_at=NULL",
        "state=?, started_at=NULL",
        "TestResetStatesOnBoot",
    ),
    (
        "J: 重名不再翻译成 ErrDuplicateName（API 会把唯一键冲突当 500）",
        STORE,
        "\t\tif isUnique(err) {\n\t\t\treturn Service{}, ErrDuplicateName\n\t\t}",
        "\t\tif false {\n\t\t\treturn Service{}, ErrDuplicateName\n\t\t}",
        "TestNameUnique",
    ),
    (
        "K: autostart 直接回显入参而不是读库（RETURNING 扫歪也测不出）",
        STORE,
        "\tsv.Autostart = autostart != 0",
        "\tsv.Autostart = true",
        "TestCreateReturnsPersistedValues",
    ),
    (
        "L: List 排序丢掉 name 次序（同 sort 下顺序随机）",
        STORE,
        "ORDER BY sort ASC, name ASC",
        "ORDER BY sort ASC",
        "TestListOrderedBySortThenName",
    ),
    (
        "M: Update 不检查 RowsAffected（改不存在的服务也报成功）",
        STORE,
        "\tif n, _ := res.RowsAffected(); n == 0 {\n\t\treturn ErrNotFound\n\t}\n\treturn nil\n}\n\n// Delete",
        "\t_ = res\n\treturn nil\n}\n\n// Delete",
        "TestUpdateMissingRow",
    ),
]

# path -> 原始内容。atexit 无条件写回，异常/Ctrl-C 也不会把仓库留在变异态。
ORIGINALS: dict[str, str] = {}


def restore_all() -> None:
    for path, content in ORIGINALS.items():
        cur = open(path).read() if __import__("os").path.exists(path) else None
        if cur != content:
            open(path, "w").write(content)
            print(f"  [已恢复 {path}]")


def run(name: str, path: str, old: str, new: str, test: str) -> bool:
    src = ORIGINALS[path]
    if old not in src:
        print(f"  !! 锚点未找到，变异未执行（测试并未通过）：{name}")
        return False
    open(path, "w").write(src.replace(old, new, 1))
    r = subprocess.run(
        ["go", "test", "-count=1", "-run", test, PKG],
        capture_output=True,
        text=True,
        timeout=300,
    )
    killed = r.returncode != 0
    detail = [ln for ln in (r.stdout + r.stderr).splitlines() if "_test.go" in ln]
    print(f"  {'被抓到' if killed else '存  活'} | {name}")
    if detail:
        print(f"        {detail[0].strip()}")
    restore_all()  # 每条变异后立刻恢复，不把风险留给下一条
    return killed


def self_test() -> int:
    """反向验证本脚本自己：往 target 里插一个必定失败的改动，
    确认被“恢复”机制擦干净。否则脚本本身就是事故源。
    """
    path = sys.argv[sys.argv.index("--self-test") + 1]
    original = open(path).read()
    atexit.register(lambda: open(path, "w").write(original))
    open(path, "w").write(original + "\npackage bogus // 故意编译失败\n")
    print(f"  已往 {path} 插入破坏性改动，立即交给 atexit 恢复")
    return 0


def main() -> int:
    if "--self-test" in sys.argv:
        return self_test()

    for _, path, *_ in MUTATIONS:
        if path not in ORIGINALS:
            ORIGINALS[path] = open(path).read()
    atexit.register(restore_all)

    survivors = []
    for mutation in MUTATIONS:
        if not run(*mutation):
            survivors.append(mutation[0])
    print()
    if survivors:
        print("以下变异未被抓到（要么补测试，要么确认它是等价变异并在此注明）：")
        for s in survivors:
            print("  -", s)
        return 1
    print("全部变异被抓到")
    return 0


if __name__ == "__main__":
    sys.exit(main())
