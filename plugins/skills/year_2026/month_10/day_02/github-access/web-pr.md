## 四b、网页操作 GitHub：发 PR 评论 + 关闭（2026-10-14 实测成功）

无 API token 时（GitHub API 不认 SSH key，只认 token），可用 browser 插件走网页。实测路线：

```bash
B="plugins/browser"
export OKH_BROWSER_AGENT=xk8451          # 多 agent 共用实例时按自己 id 隔离 tab
CTL(){ timeout 50 "$B/browserctl" <端口> "$@" 2>&1; }   # 每条单独发、串行，见 §四 坑 9
CTL open https://github.com/login
CTL type "#login_field" "<用户名>" --clear
CTL type "#password"    "<密码>"    --clear     # 密码从 credentials skill §10 取，别手打
CTL click "input[name=commit]"                  # 成功 = url 跳到 https://github.com/
CTL open  https://github.com/<owner>/<repo>/pull/<n>
CTL wait  "textarea#new_comment_field" 20000
```

**往 React 受控 textarea 灌长正文**（`type` 对几千字符 + 反引号/中文不稳）：正文先 `base64 -w0`，
再 `eval` 里解码 + 用 **native setter** + `input` 事件，实测 2204 字符一次成功：

```bash
B64=$(base64 -w0 正文.md)
CTL eval "(function(){var t=document.querySelector('textarea#new_comment_field');\
 if(!t)return 'NO_TEXTAREA';var s=decodeURIComponent(escape(atob('$B64')));\
 Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype,'value').set.call(t,s);\
 t.dispatchEvent(new Event('input',{bubbles:true}));return 'FILLED len='+t.value.length;})()"
CTL click "text:Close with comment"    # 留言+关闭一步；纯留言点 "Comment"
```

- PR 页上可见的提交按钮通常有两个：`Close with comment` 和 `Comment`（用
  `eval` 列 `button[type=submit]` 的 `textContent` + `offsetParent` 先侦察，别瞎点）。
- 正文里单写一行 `/close` 也能关闭（GitHub special comment），但**直接点 `Close with comment` 更可靠**，
  `/close` 只当双保险。
- **网页操作发出来的评论作者是登录的账号本人**（本机 = `Donald24718`），不是 agent 独立身份；
  要 agent 独立署名只能走 API+token。
- 事后用公开 API 复核，别只信页面观感：
  `curl -s https://api.github.com/repos/<owner>/<repo>/pulls/<n>` 看 `state`/`closed_at`，
  `.../issues/<n>/comments` 看最后一条的作者与长度。
