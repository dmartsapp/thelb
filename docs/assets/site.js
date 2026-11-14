/* theLB docs: copy buttons, theme toggle, mobile-nav close, "on this page" highlight.
   Everything here is progressive enhancement - the pages read fine without it. */
(function () {
  "use strict";

  // Copy buttons on code blocks.
  document.querySelectorAll(".codeblock").forEach(function (block) {
    var bar = block.querySelector(".codebar");
    if (!bar) {
      bar = document.createElement("div");
      bar.className = "codebar";
      block.insertBefore(bar, block.firstChild);
    }
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copybtn";
    btn.textContent = "Copy";
    btn.addEventListener("click", function () {
      var text = block.querySelector("pre").innerText.replace(/\n$/, "");
      var done = function () {
        btn.textContent = "Copied";
        setTimeout(function () { btn.textContent = "Copy"; }, 1400);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {});
      } else {
        var ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand("copy"); done(); } catch (e) {}
        document.body.removeChild(ta);
      }
    });
    bar.appendChild(btn);
  });

  // Theme: follow the OS by default; the button cycles auto -> light -> dark.
  var root = document.documentElement;
  var themeBtn = document.getElementById("themebtn");
  function saved() { try { return localStorage.getItem("thelb-theme"); } catch (e) { return null; } }
  function save(v) { try { v ? localStorage.setItem("thelb-theme", v) : localStorage.removeItem("thelb-theme"); } catch (e) {} }
  if (themeBtn) {
    themeBtn.addEventListener("click", function () {
      var cur = root.getAttribute("data-theme");
      var next = cur === null ? "light" : cur === "light" ? "dark" : null;
      if (next) { root.setAttribute("data-theme", next); } else { root.removeAttribute("data-theme"); }
      save(next);
      themeBtn.title = "Theme: " + (next || "auto");
    });
    themeBtn.title = "Theme: " + (saved() || "auto");
  }

  // Close the mobile navigation after choosing a page.
  var toggle = document.getElementById("nav-toggle");
  document.querySelectorAll(".sidebar a").forEach(function (a) {
    a.addEventListener("click", function () { if (toggle) toggle.checked = false; });
  });

  // Highlight the current section in "On this page".
  var links = document.querySelectorAll(".toc a");
  if (links.length && "IntersectionObserver" in window) {
    var byId = {};
    links.forEach(function (a) { byId[a.getAttribute("href").slice(1)] = a; });
    var obs = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          links.forEach(function (a) { a.classList.remove("current"); });
          var a = byId[e.target.id];
          if (a) a.classList.add("current");
        }
      });
    }, { rootMargin: "-72px 0px -70% 0px" });
    Object.keys(byId).forEach(function (id) {
      var h = document.getElementById(id);
      if (h) obs.observe(h);
    });
  }
})();
