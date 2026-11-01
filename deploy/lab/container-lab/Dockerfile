# LEVEE 容器靶场目标机镜像（本地替身：一台机器顶一台实验室 VM）
#
# 用途：让「执行期路径」在没有真 VM 集群时也能实跑——SSH 认证、文件下发、
# 服务管理、批间门禁、失败回滚、快照采集与还原。
#
# 装的东西：
#   openssh  —— SSH 服务（密码认证，root + lab 两个账号）
#   nginx    —— 被演示的工作流改它的配置（stub_status 提供 /nginx-status 探针）
#   openrc   —— 服务管理器。**没有 openrc 就没有 `service` 命令**，
#               svc 模块会退回 sysvinit 分支去跑 `service nginx start`。
#
# 两个必须记住的坑（都踩过）：
#   1. openrc 在没 boot 过的容器里会拒绝起服务，报
#      "failed to acquire lock: Bad file descriptor" 并提示
#      `touch /run/openrc/softlevel` —— 这里已经把目录和文件建好了。
#   2. openrc 的 pidfile 是 /run/nginx/nginx.pid，而 nginx 自己默认写
#      /run/nginx.pid。两者不一致时 `service nginx status` 会报 "crashed"，
#      svc 模块的 isActive 判据就废了 —— 所以下面把 nginx.conf 的 pid
#      指令改成 openrc 期望的路径。
#
# 另：Alpine 的 busybox 把 **pgrep 做成了 pkill 的别名**。
#    `pgrep nginx` 不是"查进程"，是"按名字杀进程"（连执行它的 shell 一起杀）。
#    存活探测请用 `service nginx status`（systemd 与 openrc 语义一致）。
FROM alpine:3.24
RUN apk add --no-cache openssh nginx openrc >/dev/null
RUN adduser -D -s /bin/sh lab && echo 'lab:labpass' | chpasswd \
 && echo 'root:rootpass' | chpasswd \
 && ssh-keygen -A \
 && printf 'PasswordAuthentication yes\nPermitRootLogin yes\nAllowUsers root lab\nUseDNS no\n' > /etc/ssh/sshd_config \
 && mkdir -p /run/sshd /var/www/lab /run/openrc /run/nginx \
 && touch /run/openrc/softlevel \
 && sed -i '1i pid /run/nginx/nginx.pid;' /etc/nginx/nginx.conf \
 && printf 'server {\n  listen 80;\n  server_name _;\n  root /var/www/lab;\n  index index.html;\n  location /nginx-status {\n    stub_status;\n  }\n}\n' > /etc/nginx/http.d/lab.conf \
 && rm -f /etc/nginx/http.d/default.conf \
 && printf '# preseeded known-good config; the rollback proof overwrites it and expects it back\nserver {\n  listen 8080;\n  server_name preseeded;\n  root /var/www/lab;\n}\n' > /etc/nginx/http.d/lab-site.conf \
 && chmod -R a+rX /var/www/lab
EXPOSE 22 80
CMD ["/usr/sbin/sshd", "-D", "-e"]
