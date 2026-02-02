#!/usr/bin/env python3
"""
批量下载快手视频脚本
从 res-downloader 导出的 txt 文件中提取 URL，使用 wget 下载。
已存在的文件会自动跳过。
"""

import urllib.parse
import json
import os
import subprocess
import sys
import re
import time

EXPORT_FILE = "/Users/bolin/Documents/AI/claude_code/res-downloader/res-downloader-20260201152239.txt"
DOWNLOAD_DIR = "/Users/bolin/Downloads"
MAX_CONCURRENT = 5  # 并发下载数


def sanitize_filename(name, max_len=200):
    """清理文件名，去除不安全字符"""
    # Remove '#' symbols but keep the text content
    name = name.replace('#', ' ')
    # Normalize non-breaking spaces
    name = name.replace('\xa0', ' ')
    # Replace unsafe chars
    name = re.sub(r'[/\\:*?"<>|]', '_', name)
    # Collapse whitespace
    name = re.sub(r'\s+', '_', name).lstrip('_')
    if len(name) > max_len:
        name = name[:max_len]
    return name


def parse_export_file(filepath):
    """解析导出文件，返回视频列表"""
    videos = []
    with open(filepath, 'r', encoding='utf-8') as f:
        for i, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                decoded = urllib.parse.unquote(line)
                obj = json.loads(decoded)
                url = obj.get('Url', '')
                if not url:
                    continue
                desc = obj.get('Description', '')
                suffix = obj.get('Suffix', '.mp4')
                save_path = obj.get('SavePath', '')

                # Extract timestamp from SavePath (e.g. _20260201145613)
                timestamp = ''
                if save_path:
                    ts_match = re.search(r'_(\d{14})', save_path)
                    if ts_match:
                        timestamp = f"_{ts_match.group(1)}"

                if desc:
                    name = sanitize_filename(desc)
                else:
                    # Fallback to SavePath filename or index
                    if save_path:
                        name = os.path.splitext(os.path.basename(save_path))[0]
                        timestamp = ''  # already in name
                    else:
                        name = f"video_{i}"

                filename = f"{name}{timestamp}{suffix}"

                other_data = obj.get('OtherData', {})
                like_count = other_data.get('likeCount', '')

                videos.append({
                    'url': url,
                    'filename': filename,
                    'index': i,
                    'like_count': like_count,
                })
            except (json.JSONDecodeError, Exception) as e:
                print(f"  [WARN] Line {i}: parse error: {e}")
    return videos


def download_video(video, download_dir):
    """下载单个视频"""
    filepath = os.path.join(download_dir, video['filename'])

    # Skip if already exists and has content
    if os.path.exists(filepath) and os.path.getsize(filepath) > 0:
        return 'skipped'

    try:
        result = subprocess.run(
            [
                'wget', '-q', '--no-check-certificate',
                '-O', filepath,
                '--timeout=60',
                '--tries=3',
                '--waitretry=5',
                video['url']
            ],
            capture_output=True,
            text=True,
            timeout=300  # 5 min max per video
        )
        if result.returncode == 0 and os.path.exists(filepath) and os.path.getsize(filepath) > 0:
            return 'success'
        else:
            # Clean up empty/failed files
            if os.path.exists(filepath) and os.path.getsize(filepath) == 0:
                os.remove(filepath)
            return 'failed'
    except subprocess.TimeoutExpired:
        if os.path.exists(filepath) and os.path.getsize(filepath) == 0:
            os.remove(filepath)
        return 'timeout'
    except Exception as e:
        return f'error: {e}'


def main():
    print(f"解析导出文件: {EXPORT_FILE}")
    videos = parse_export_file(EXPORT_FILE)
    print(f"共 {len(videos)} 条视频记录")

    # Check which ones already exist
    to_download = []
    skipped = 0
    for v in videos:
        filepath = os.path.join(DOWNLOAD_DIR, v['filename'])
        if os.path.exists(filepath) and os.path.getsize(filepath) > 0:
            skipped += 1
        else:
            to_download.append(v)

    print(f"已存在: {skipped}, 需下载: {len(to_download)}")

    if not to_download:
        print("所有视频都已下载完成！")
        return

    success = 0
    failed = 0
    for i, video in enumerate(to_download, 1):
        like_info = f" (♥ {video['like_count']})" if video.get('like_count') else ""
        print(f"[{i}/{len(to_download)}] 下载: {video['filename'][:60]}{like_info}...", end=' ', flush=True)
        result = download_video(video, DOWNLOAD_DIR)
        if result == 'success':
            success += 1
            print("OK")
        elif result == 'skipped':
            skipped += 1
            print("跳过(已存在)")
        else:
            failed += 1
            print(f"失败({result})")

        # Brief pause between downloads
        if i < len(to_download):
            time.sleep(0.5)

    print(f"\n下载完成！成功: {success}, 失败: {failed}, 跳过: {skipped}")

    if failed > 0:
        print("提示: 失败的文件可重新运行此脚本重试")


if __name__ == '__main__':
    main()
