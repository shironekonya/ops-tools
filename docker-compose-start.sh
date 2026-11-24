#!/bin/bash

# 改成你3个项目的真实目录
COMPOSE_DIRS=(
"/data/jenkins-data"
"/data/harbor"
"/data/jenkins-exportor"
)

echo "===== 批量启动 compose ====="

for dir in "${COMPOSE_DIRS[@]}"; do
  echo -e "\n----------------------------------------"
  echo "处理目录: $dir"
  if [ ! -d "$dir" ];then
    echo "⚠️ 目录不存在，跳过: $dir"
    continue
  fi
  cd "$dir" || exit 1

  # 自动判断用哪个文件
  if [ -f "docker-compose.yml" ]; then
    COMPOSE_FILE="docker-compose.yml"
  elif [ -f "docker-compose.yaml" ]; then
    COMPOSE_FILE="docker-compose.yaml"
  else
    echo "❌ $dir 下找不到 docker-compose.yml / .yaml，跳过"
    continue
  fi

  echo "使用配置文件: $COMPOSE_FILE"
  docker-compose -f "$COMPOSE_FILE" up -d
  echo "✅ $dir 启动完成"
done

echo -e "\n===== 全部执行完毕 ====="
docker ps
