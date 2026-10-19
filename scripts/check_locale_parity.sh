#!/usr/bin/env bash
# check_locale_parity.sh —— 7 语种词典奇偶校验（web-00 §7.6 / TC-C-04）
#
# 为什么需要这个脚本：UI 文案以 `en` 为权威基线，其余 6 语是逐键翻译。缺一个键不会
# 让构建失败，却会在运行时静默回退到英文（§7.2），所以必须在 CI 用机械断言拦住：
#   1. canonical 7 语目录齐全（en / zh-CN / zh-TW / fr / ru / ja / vi）；
#   2. 每种语言的域文件集合与 en 完全一致（多一个文件或少一个文件都失败）；
#   3. 每个域展开后的键集合与 en 完全一致（缺键/多键都失败）；
#   4. 所有值均为非空字符串；
#   5. 占位符集合（{name} 形式）与 en 一致——翻译时漏掉 {count} 会让界面显示残缺。
#
# 用法：bash scripts/check_locale_parity.sh
# 退出码：0 通过；1 存在不一致。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCALES="$ROOT/frontend/src/locales"

if [ ! -d "$LOCALES" ]; then
  echo "FAIL 找不到词典目录：$LOCALES" >&2
  exit 1
fi

exec python3 - "$LOCALES" <<'PY'
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
canonical = ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi']
baseline = 'en'
failures = []


def flatten(prefix, value, out):
    for key, item in value.items():
        full = f'{prefix}.{key}' if prefix else key
        if isinstance(item, dict):
            flatten(full, item, out)
        elif isinstance(item, str):
            out[full] = item
        else:
            failures.append(f'{prefix}.{key}: 值必须是字符串，实际为 {type(item).__name__}')


def placeholders(text):
    return set(re.findall(r'\{([a-zA-Z0-9_]+)\}', text))


# 1. canonical 目录齐全
for lang in canonical:
    if not (root / lang).is_dir():
        failures.append(f'缺少语言目录：{lang}')

# 2. 域文件集合与 en 一致
en_dir = root / baseline
if not en_dir.is_dir():
    failures.append('缺少 en 基线目录')
    print('\n'.join(failures), file=sys.stderr)
    sys.exit(1)

domains = sorted(p.name for p in en_dir.glob('*.json'))
if not domains:
    failures.append('en 目录下没有任何域文件')

for lang in canonical:
    lang_dir = root / lang
    if not lang_dir.is_dir():
        continue
    other = sorted(p.name for p in lang_dir.glob('*.json'))
    missing = sorted(set(domains) - set(other))
    extra = sorted(set(other) - set(domains))
    if missing:
        failures.append(f'{lang}: 缺域文件 {", ".join(missing)}')
    if extra:
        failures.append(f'{lang}: 多出域文件 {", ".join(extra)}')

# 3/4/5. 逐域展开比较
for domain in domains:
    en_path = en_dir / domain
    try:
        en_flat = {}
        flatten('', json.loads(en_path.read_text(encoding='utf-8')), en_flat)
    except (OSError, json.JSONDecodeError) as error:
        failures.append(f'{baseline}/{domain}: 无法解析（{error}）')
        continue

    for lang in canonical:
        if lang == baseline:
            continue
        path = root / lang / domain
        if not path.is_file():
            continue
        try:
            flat = {}
            flatten('', json.loads(path.read_text(encoding='utf-8')), flat)
        except (OSError, json.JSONDecodeError) as error:
            failures.append(f'{lang}/{domain}: 无法解析（{error}）')
            continue

        missing = sorted(set(en_flat) - set(flat))
        extra = sorted(set(flat) - set(en_flat))
        empty = sorted(key for key, value in flat.items() if value.strip() == '')
        if missing:
            failures.append(f'{lang}/{domain}: 缺键 {", ".join(missing[:10])}' +
                            (f' 等 {len(missing)} 个' if len(missing) > 10 else ''))
        if extra:
            failures.append(f'{lang}/{domain}: 多键 {", ".join(extra[:10])}' +
                            (f' 等 {len(extra)} 个' if len(extra) > 10 else ''))
        if empty:
            failures.append(f'{lang}/{domain}: 空值 {", ".join(empty[:10])}' +
                            (f' 等 {len(empty)} 个' if len(empty) > 10 else ''))

        for key in sorted(set(en_flat) & set(flat)):
            expected = placeholders(en_flat[key])
            actual = placeholders(flat[key])
            if expected != actual:
                failures.append(
                    f'{lang}/{domain}:{key}: 占位符不一致（en={sorted(expected)} {lang}={sorted(actual)}）'
                )

if failures:
    print('FAIL 词典奇偶校验未通过：', file=sys.stderr)
    for failure in failures:
        print(f'  - {failure}', file=sys.stderr)
    sys.exit(1)


print(f'PASS {len(canonical)} 语种 × {len(domains)} 域键集合一致（{", ".join(domains)}）')
PY
