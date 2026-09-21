#!/usr/bin/env bash
#
# 生成群晖「套件来源」的静态 feed（.gh-pages/index.json），由 gh-pages 分支托管。
#
#   ./spk/gen-feed.sh <spk 文件> <下载直链> [输出路径]
#
# 例：
#   ./spk/gen-feed.sh agg-api-dsm-0.1.0.spk \
#     https://github.com/kosje/Agg-API-DSM/releases/download/v0.1.0/agg-api-dsm-0.1.0.spk
#
# 输出路径缺省是 `.gh-pages/index.json` —— GitHub Pages 服务的是 gh-pages 分支
# 根目录，所以真正生效的是那份，不是 docs/ 下的。两条路径各写各的会导致
# 「Pages 读到的」和「脚本生成的」不一致，所以这里只写一份。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPKFILE="${1:?用法: gen-feed.sh <spk 文件> <下载直链> [输出路径]}"
LINK="${2:?用法: gen-feed.sh <spk 文件> <下载直链> [输出路径]}"
OUTFILE="${3:-$ROOT/.gh-pages/index.json}"
OUTDIR="$(dirname "$OUTFILE")"
PAGES_BASE="${PAGES_BASE:-https://kosje.github.io/Agg-API-DSM}"

[ -f "$SPKFILE" ] || { echo "找不到 $SPKFILE"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "需要 jq"; exit 1; }

# 直接从 SPK 里读 INFO，保证 feed 与包内元数据永远一致 ——
# 手工维护一份副本迟早会漂移。
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
tar xf "$SPKFILE" -C "$TMP" INFO

info() { grep -m1 "^$1=" "$TMP/INFO" | cut -d'"' -f2 || true; }

PKG="$(info package)"
VER="$(info version)"
DNAME="$(info displayname)"
DESC="$(info description)"
MAINT="$(info maintainer)"
MAINT_URL="$(info maintainer_url)"
ARCH="$(info arch)"
OSMIN="$(info os_min_ver)"

SIZE="$(wc -c < "$SPKFILE" | tr -d ' ')"
MD5="$(md5sum "$SPKFILE" | cut -d' ' -f1)"

# 图标必须与 feed 同目录：thumbnail 用的是 Pages 上的绝对地址。
mkdir -p "$OUTDIR"
cp -f "$ROOT/spk/ui/images/icon_72.png" "$OUTDIR/icon_72.png"
cp -f "$ROOT/spk/ui/images/icon_256.png" "$OUTDIR/icon_256.png"

CHANGELOG="${CHANGELOG:-详见 $MAINT_URL/releases}"

# qinst/qstart 表示「无需向导即可安装 / 启动」。
# 本套件带安装向导（要设管理端口令），所以两者都必须是 false ——
# 写成 true 会让套件中心跳过向导，口令就无从设置了。
jq -n \
  --arg package "$PKG" --arg version "$VER" --arg dname "$DNAME" \
  --arg desc "$DESC" --arg link "$LINK" --arg md5 "$MD5" \
  --argjson size "$SIZE" \
  --arg maintainer "$MAINT" --arg maintainer_url "$MAINT_URL" \
  --arg changelog "$CHANGELOG" \
  --arg thumb "$PAGES_BASE/icon_72.png" \
  --arg thumb2x "$PAGES_BASE/icon_256.png" \
  '{
     packages: [{
       package: $package,
       version: $version,
       dname: $dname,
       desc: $desc,
       link: $link,
       md5: $md5,
       size: $size,
       thumbnail: [$thumb],
       thumbnail_retina: [$thumb2x],
       qinst: false,
       qupgrade: true,
       qstart: false,
       snapshot: [],
       maintainer: $maintainer,
       maintainer_url: $maintainer_url,
       distributor: $maintainer,
       distributor_url: $maintainer_url,
       changelog: $changelog
     }]
   }' > "$OUTFILE"

# arch / os_min_ver 默认不写：SPK 的 INFO.arch 已在安装期做校验（ARM 机型会被
# 套件中心直接拒绝安装），而 feed 里这两个字段的期望类型没有把握 ——
# 写错会让套件中心整个读不到本 feed，代价远大于收益。
# 确认无误后可用 FEED_EMIT_ARCH=1 打开。
if [ "${FEED_EMIT_ARCH:-0}" = "1" ]; then
    jq --arg arch "$ARCH" --arg osmin "$OSMIN" \
       '.packages[0].arch = $arch | .packages[0].os_min_ver = $osmin' \
       "$OUTFILE" > "$OUTFILE.tmp" && mv "$OUTFILE.tmp" "$OUTFILE"
fi

jq empty "$OUTFILE"
echo "==> 已生成 $OUTFILE"
echo "    套件: $PKG $VER   arch=$ARCH   os_min_ver=$OSMIN"
echo "    大小: $SIZE 字节   md5: $MD5"
echo "    下载: $LINK"
echo
echo "套件来源地址（套件中心 → 设置 → 套件来源 → 新增）:"
echo "    $PAGES_BASE/index.json"
