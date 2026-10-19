/**
 * HTML 净化（2026-10-19）
 *
 * 为什么要：bot 回复按 markdown 渲染（md() → marked.parse → innerHTML）。marked
 * 原样放行原始 HTML——实测这些都会直通：
 *   空行包住的 <img src=x onerror=…>、<svg onload=…>、<iframe src="data:…">、
 *   <details><summary onclick=…>，以及 [点我](javascript:…) 这种协议不过期的链接。
 * 会话内容不是主人自己写的：网页、文件、群消息经工具进模型，模型可能原样复述其中
 * 的标签。插进 innerHTML 就是 XSS——而本机 API 没有鉴权，脚本能以页面身份
 * POST /enqueue 给 agent 下指令、POST /config 改配置、POST /reset 清会话。
 * 外部内容由此从"骗模型"升级成"控制实例"。
 *
 * 做法：解析后删可执行/可外联的标签，删所有 on* 与 style，URL 属性走协议白名单。
 * 不引第三方库（本机离线，且多一个依赖多一份风险）。
 */
(function (global) {
  // 可执行脚本 / 外联容器 / 可伪造 UI 的标签，整棵子树删掉
  var KILL = "script,style,iframe,frame,frameset,object,embed,link,meta,base,form,input,button,textarea,select,portal,svg,math,annotation-xml,template";
  var URL_ATTRS = { href: 1, src: 1, "xlink:href": 1, action: 1, formaction: 1, srcdoc: 1, data: 1, poster: 1, background: 1 };

  function safeUrl(v, attr) {
    // 去掉空白与控制字符再判协议：javascript:\talert(1) 这类绕法要挡掉
    var s = String(v).replace(/[\s\u0000-\u001f]/g, "").toLowerCase();
    if (/^(#|https?:|mailto:|tel:)/.test(s)) return true;
    if (attr === "src" && /^data:image\/(png|jpe?g|gif|webp);base64,/.test(s)) return true;
    // 站内绝对路径（/foo）可以；协议相对（//evil.com）不行
    if (s.charAt(0) === "/" && s.charAt(1) !== "/") return true;
    return false;
  }

  function sanitizeHtml(html) {
    var doc = new global.DOMParser().parseFromString("<body>" + String(html == null ? "" : html) + "</body>", "text/html");
    var dead = doc.querySelectorAll(KILL);
    for (var i = 0; i < dead.length; i++) dead[i].parentNode.removeChild(dead[i]);
    var all = doc.querySelectorAll("*");
    for (var j = 0; j < all.length; j++) {
      var el = all[j];
      var attrs = Array.prototype.slice.call(el.attributes);
      for (var k = 0; k < attrs.length; k++) {
        var name = attrs[k].name.toLowerCase();
        if (name.indexOf("on") === 0 || name === "style") { el.removeAttribute(attrs[k].name); continue; }
        if (URL_ATTRS[name] && !safeUrl(attrs[k].value, name)) el.removeAttribute(attrs[k].name);
      }
    }
    return doc.body.innerHTML;
  }

  global.sanitizeHtml = sanitizeHtml;
})(typeof window !== "undefined" ? window : globalThis);
