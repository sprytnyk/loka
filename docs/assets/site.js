// Loka's website: download buttons for your system and the terminal that types.
(() => {
  "use strict";

  const lang = document.documentElement.lang === "uk" ? "uk" : "en";
  const REPO = "sprytnyk/loka";
  const calm = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  const TEXT = {
    en: {
      linux: "Download for Linux",
      windows: "Download for Windows",
      mac_arm64: "macOS · Apple silicon",
      mac_x64: "macOS · Intel",
      linux_short: "Linux",
      windows_short: "Windows",
      all: "All downloads",
      version: (v, d) => `Version ${v} came out on ${d}. Loka is free and open source under the MIT licence.`,
    },
    uk: {
      linux: "Завантажити для Linux",
      windows: "Завантажити для Windows",
      mac_arm64: "macOS · Apple silicon",
      mac_x64: "macOS · Intel",
      linux_short: "Linux",
      windows_short: "Windows",
      all: "Усі завантаження",
      version: (v, d) => `Версія ${v} вийшла ${d}. Loka безкоштовна й має відкритий код за ліцензією MIT.`,
    },
  }[lang];

  const el = (tag, cls, text) => {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  };

  const remember = (key, value) => {
    try {
      localStorage.setItem(key, value);
    } catch {
      // private mode: the choice just isn't kept
    }
  };

  // What the terminal shows. The output is what Loka prints, with documentation
  // addresses (192.0.2.x) and a made-up link. The QR code is a real one for that link,
  // drawn by Loka's own encoder.
  const LINK = "Xq7mR2vLpT9cWb4nHs6kJd1fGz8yAe3u";
  const QR = `█████████████████████████████████████████
█████████████████████████████████████████
████ ▄▄▄▄▄ █▄▄▄▄█ ▀▄▀ ▀▀ ▀▄▄ █ ▄▄▄▄▄ ████
████ █   █ █▀ █▄▄█▄  ▄█▀▀▄▀█ █ █   █ ████
████ █▄▄▄█ █  ██ ▄█▀▄▀█▀█▀▄▄▀█ █▄▄▄█ ████
████▄▄▄▄▄▄▄█ ▀▄█ █ █▄▀ ▀ ▀ ▀▄█▄▄▄▄▄▄▄████
████▄█▀█ █▄  ▄   █▀   ▄▄▀▄█  ▄▄▄▄ ▀█▄████
████▀ ▄ █▀▄▄▀█▀   ▄   ██ ▀▀ █▀▀██▄█▄▀████
████▀▀▄ █ ▄▀███▄█ ▀▄█▄█ ▄ ▄█▄█ ▀▄ ▀▄▄████
██████▄ ▀ ▄▀▄ █▀    █▀▀▀▀▀▀▄▀▀█▀▄▄▄▄█████
████ ▄▄ █▀▄██   ▄ ▄█   ▄▀ ▀ ▀▄▄█▄▀ ▄█████
████▄█▀▀▄▀▄▀ █▄▄█▀  █ ▀█▀▀▄▄▀ ▀██▄██▀████
████▀█▄▄██▄█▀ █▀▀▀▄  ▀▀▄▄█▄ ▄▀▄▄▄ █ ▄████
███████ █▀▄▀█▀▄▄█▀█▄▀▀ █▄▀  ▀▀▄▄ ▄█ ▀████
████▄▄█▄██▄█ ▄█  ▄█▀▄▄ ▄▀ ▀█ ▄▄▄ ▄▀▄▀████
████ ▄▄▄▄▄ █▄▀ ▀ ▀█ ▄ ▀▀█▀ ▀ █▄█  █ █████
████ █   █ █▄▀███▄█▀▄▄▀█▀▄▄█▄  ▄ ▄▀██████
████ █▄▄▄█ ██  ▀██▄ █▀▀█▀  ▄▀▄▄ ▄▄███████
████▄▄▄▄▄▄▄█▄█▄▄█▄▄██▄█▄█▄██▄▄██▄▄▄█▄████
█████████████████████████████████████████
▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀`;
  const SCRIPT = [
    { cmd: "loka devices" },
    { out: "Studio Laptop  192.0.2.21:45873  (remembered)" },
    { out: "Living Room PC  192.0.2.22:45873" },
    { cmd: "loka send --to 192.0.2.21 ~/Videos/holiday.mp4" },
    { out: "Pairing code 482 913. Check that the receiver shows the same." },
    {
      progress: [
        "waiting: 0%",
        "sending: 18% at 296.4 Mbps",
        "sending: 41% at 308.9 Mbps",
        "sending: 67% at 311.2 Mbps",
        "sending: 89% at 312.0 Mbps",
        "complete: 100% at 312.4 Mbps",
      ],
    },
    { cmd: "loka share ~/Documents/itinerary.pdf" },
    { out: "Open this on a phone or in a browser:" },
    { qr: QR },
    { url: `http://192.0.2.20:45873/#d=${LINK}` },
    { out: "Safer, once the phone has Loka's certificate:" },
    { url: `https://192.0.2.20:45873/#d=${LINK}` },
    {
      out:
        "Loka's certificate fingerprint (SHA-256): 3F 9A 1C 72 E4 0B 5D 88 A1 6E 2F 93 C7 D0 4B 19 " +
        "7E 55 A8 0C 31 F6 9D 2B E8 47 C3 6A 10 BD 92 5F",
    },
    { out: "With curl:" },
    {
      out:
        "curl -kOJ --pinnedpubkey sha256//q3Tn8vYc0bJxR5mW2eLhK7uPz9sAf4GdN1oVi6CwXyE= " +
        `https://192.0.2.20:45873/download/${LINK}`,
    },
  ];

  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  // The copy of Loka's page at the top. Its words are Loka's own, in English or Ukrainian.
  const plural = new Intl.PluralRules(lang);
  const APP = {
    en: {
      selected: { one: "{n} item selected", other: "{n} items selected" },
      names: { one: "{n} name", other: "{n} names" },
      characters: { one: "{n} character", other: "{n} characters" },
      nothing: "Nothing selected",
      first: "Choose files or directories first.",
      enterText: "Type or paste some text first.",
      textTooLong: "The text is longer than 1 MB. Send it as a file instead.",
      pasteFailed: "Could not read the clipboard. Paste into the box instead.",
      textPreview: "Text: {text}",
      remove: "Remove",
      to: "To {name}",
      statuses: { waiting: "waiting", sending: "sending", receiving: "receiving", complete: "complete", cancelled: "cancelled" },
      speed: "{speed} Mbps",
      cancel: "Cancel",
      code: "Pairing code {code}. Check that the other device shows the same.",
      secure: "Safer, once the phone has Loka's certificate:",
      qr: "QR code",
      qrFor: "QR code for {url}",
      downloadQr: "Download on a phone",
      textQr: "Open on a phone",
      phoneQr: "Receive from a phone",
      demoAddress: "This QR code uses an example address.",
      help: "Loka encrypts transfers. Compare the pairing codes on both computers the first time you send. Phones can use Loka's certificate to check which computer answered.",
      helpMore: "Read about certificates and pairing codes",
      stop: "Stop sharing",
    },
    uk: {
      selected: { one: "Вибрано {n} елемент", few: "Вибрано {n} елементи", many: "Вибрано {n} елементів", other: "Вибрано {n} елемента" },
      names: { one: "{n} назва", few: "{n} назви", many: "{n} назв", other: "{n} назви" },
      characters: { one: "{n} символ", few: "{n} символи", many: "{n} символів", other: "{n} символу" },
      nothing: "Нічого не вибрано",
      first: "Спершу виберіть файли або теки.",
      enterText: "Спершу введіть або вставте текст.",
      textTooLong: "Текст довший за 1 МБ. Надішліть його файлом.",
      pasteFailed: "Не вдалося прочитати буфер обміну. Вставте текст у поле вручну.",
      textPreview: "Текст: {text}",
      remove: "Прибрати",
      to: "До {name}",
      statuses: { waiting: "очікує", sending: "надсилання", receiving: "отримання", complete: "завершено", cancelled: "скасовано" },
      speed: "{speed} Мбіт/с",
      cancel: "Скасувати",
      code: "Код з’єднання {code}. Перевірте, що інший пристрій показує такий самий.",
      secure: "Безпечніше, коли на телефоні вже є сертифікат Loka:",
      qr: "QR-код",
      qrFor: "QR-код для {url}",
      downloadQr: "Завантажити на телефон",
      textQr: "Відкрити на телефоні",
      phoneQr: "Отримати з телефона",
      demoAddress: "У цьому QR-коді наведено адресу для прикладу.",
      help: "Loka шифрує передавання. Першого разу звірте коди з’єднання на обох комп’ютерах. Сертифікат Loka дає телефону змогу перевірити, який комп’ютер відповів.",
      helpMore: "Про сертифікати й коди з’єднання",
      stop: "Закрити доступ",
    },
  }[lang];
  const say = (key, vars = {}) => {
    let text = APP[key];
    if (typeof text === "object") text = text[plural.select(vars.n)] || text.other;
    return text.replace(/\{(\w+)\}/g, (_, k) => vars[k]);
  };
  const ASSETS = new URL(".", document.currentScript.src);

  function app() {
    const root = document.querySelector("[data-app]");
    if (!root) return;
    const $ = (sel) => root.querySelector(sel);
    const button = (label, cls, onClick) => {
      const b = el("button", `m-btn ${cls}`, label);
      b.type = "button";
      b.addEventListener("click", onClick);
      return b;
    };
    const base = (path) => path.split("/").filter(Boolean).pop();

    // the sun and moon switch only this window, as they switch only Loka's page
    $("[data-theme-toggle]").addEventListener("click", () => {
      const dark =
        root.dataset.theme === "dark" ||
        (!root.dataset.theme && matchMedia("(prefers-color-scheme: dark)").matches);
      root.dataset.theme = dark ? "light" : "dark";
    });

    const dialog = $("[data-demo-dialog]");
    const showDialog = (title, ...contents) => {
      $("[data-dialog-title]").textContent = title;
      $("[data-dialog-body]").replaceChildren(...contents);
      dialog.showModal();
    };
    const help = $("[data-demo-help]");
    help.addEventListener("click", () => {
      const more = el("a", "", say("helpMore"));
      more.href = "#faq";
      more.addEventListener("click", () => dialog.close());
      showDialog(help.getAttribute("aria-label"), el("p", "", say("help")), more);
    });

    // Items to share
    const list = $("[data-selected]");
    const count = $("[data-count]");
    const editor = $("[data-text]");
    const textCount = $("[data-text-count]");
    const kinds = [...root.querySelectorAll("[data-kind]")];
    let kind = "files";
    const encode = new TextEncoder();
    const maxText = 1 << 20;
    const countText = () => {
      textCount.classList.remove("warn");
      textCount.textContent = editor.value ? say("characters", { n: [...editor.value].length }) : "";
    };
    for (const b of kinds) {
      b.addEventListener("click", () => {
        kind = b.dataset.kind;
        for (const control of kinds) control.setAttribute("aria-pressed", control === b);
        $("[data-files-panel]").hidden = kind === "text";
        $("[data-text-panel]").hidden = kind === "files";
        if (kind === "text") editor.focus({ preventScroll: true });
      });
    }
    editor.addEventListener("input", countText);
    const paste = $("[data-paste]");
    paste.hidden = !navigator.clipboard?.readText;
    paste.addEventListener("click", async () => {
      try {
        const text = await navigator.clipboard.readText();
        editor.setRangeText(text, editor.selectionStart, editor.selectionEnd, "end");
        countText();
      } catch {
        textCount.classList.add("warn");
        textCount.textContent = say("pasteFailed");
      }
      editor.focus({ preventScroll: true });
    });

    const examples = {
      files: ["/home/alex/Music/live-set.flac", "/home/alex/Downloads/manual.pdf", "/home/alex/Documents/itinerary.pdf"],
      directory: ["/home/alex/Projects/website", "/home/alex/Documents/travel", "/home/alex/Pictures/weekend-photos"],
    };
    const selected = () => [...list.querySelectorAll("li > span")].map((s) => s.textContent);
    const counted = () => {
      const n = selected().length;
      count.classList.remove("warn");
      count.textContent = n ? say("selected", { n }) : say("nothing");
    };
    const add = (path) => {
      if (!path || selected().includes(path)) return;
      const li = el("li");
      li.append(el("span", "", path), button(say("remove"), "sec small", () => remove(li)));
      list.append(li);
      counted();
    };
    const remove = (li) => {
      li.remove();
      counted();
    };
    for (const li of list.children) {
      li.querySelector("button").addEventListener("click", () => remove(li));
    }
    for (const b of root.querySelectorAll("[data-choose]")) {
      b.addEventListener("click", () => add(examples[b.dataset.choose].find((p) => !selected().includes(p))));
    }
    const drop = $("[data-drop]");
    drop.addEventListener("dragover", (e) => {
      e.preventDefault();
      drop.classList.add("over");
    });
    drop.addEventListener("dragleave", (e) => {
      if (!drop.contains(e.relatedTarget)) drop.classList.remove("over");
    });
    drop.addEventListener("drop", (e) => {
      e.preventDefault();
      drop.classList.remove("over");
      for (const file of e.dataTransfer.files) add(file.name);
    });
    $("[data-add]").addEventListener("submit", (e) => {
      e.preventDefault();
      const input = e.currentTarget.querySelector("input");
      add(input.value.trim());
      input.value = "";
    });
    const payload = () => {
      if (kind === "text") {
        const text = editor.value;
        if (text && encode.encode(text).length <= maxText) return { text };
        textCount.classList.add("warn");
        textCount.textContent = say(text ? "textTooLong" : "enterText");
        return null;
      }
      const names = selected().map(base);
      if (names.length) return { names };
      count.textContent = say("first");
      count.classList.add("warn");
      return null;
    };

    // Transfers
    const box = $("[data-transfers]");
    const speedText = (mbps) =>
      say("speed", {
        speed: mbps.toLocaleString(lang === "uk" ? "uk-UA" : "en-GB", {
          minimumFractionDigits: 1,
          maximumFractionDigits: 1,
        }),
      });

    function showContents(item, { names, text }) {
      if (text !== undefined) {
        const chars = [...text];
        const preview = chars.slice(0, 120).join("").replace(/\s+/g, " ").trim();
        item.append(el("small", "m-preview", say("textPreview", { text: preview + (chars.length > 120 ? "…" : "") })));
      } else if (names.length === 1) {
        const p = el("p");
        p.append(el("small", "", names[0]));
        item.append(p);
      } else {
        const files = el("details", "m-files");
        files.append(el("summary", "", say("names", { n: names.length })));
        const ul = el("ul");
        for (const name of names) ul.append(el("li", "", name));
        files.append(ul);
        item.append(files);
      }
    }

    function transfer(title, contents, card) {
      const t = { status: "waiting", progress: 0, speed: null, stopped: false };
      const item = card || el("div", "m-item enter");
      item.textContent = "";
      const row = el("div", "m-row");
      const status = el("span", "m-status");
      row.append(el("strong", "", title), status);
      item.append(row);
      showContents(item, contents);
      const code = el("p", "m-code");
      const bar = el("div", "m-bar");
      const fill = el("i");
      bar.append(fill);
      const cancel = button(say("cancel"), "sec", () => {
        t.stopped = true;
        t.status = "cancelled";
        t.speed = null;
        draw();
      });
      item.append(code, bar, cancel);
      if (!card) box.prepend(item);

      function draw() {
        const active = t.status === "sending" || t.status === "receiving";
        const speed = t.speed === null ? "" : ` · ${speedText(t.speed)}`;
        status.textContent = `${APP.statuses[t.status]} · ${Math.round(t.progress)}%${speed}`;
        code.textContent = t.code ? say("code", { code: t.code }) : "";
        code.hidden = !t.code;
        bar.hidden = !active;
        fill.style.width = `${t.progress}%`;
        cancel.hidden = !active;
      }

      // moves from where it is to 100%, around the given speed
      t.run = async (status, mbps) => {
        t.code = null;
        if (contents.text !== undefined) {
          t.status = "complete";
          t.progress = 100;
          draw();
          return;
        }
        t.status = status;
        while (!t.stopped && t.progress < 100) {
          t.speed = mbps * (0.94 + Math.random() * 0.08);
          draw();
          await sleep(calm ? 0 : 450);
          t.progress = Math.min(100, t.progress + (calm ? 100 : 3 + Math.random() * 6));
        }
        if (t.stopped) return;
        t.status = "complete";
        t.speed = mbps * (0.98 + Math.random() * 0.02);
        draw();
      };
      t.draw = draw;
      draw();
      return t;
    }

    async function send(name, mbps) {
      const contents = payload();
      if (!contents) return;
      const t = transfer(say("to", { name }), contents);
      t.code = String(100000 + Math.floor(Math.random() * 900000)).replace(/(\d{3})(\d{3})/, "$1 $2");
      t.draw();
      await sleep(calm ? 0 : 1800);
      if (t.stopped) return;
      await t.run("sending", mbps);
      if (t.status === "complete" && editor.value === contents.text) {
        editor.value = "";
        countText();
      }
    }
    for (const b of root.querySelectorAll("[data-send]")) {
      b.addEventListener("click", () => send(b.dataset.send, Number(b.dataset.speed)));
    }
    $("[data-manual]").addEventListener("submit", (e) => {
      e.preventDefault();
      const { address, port } = e.currentTarget.elements;
      const host = address.value.trim();
      if (!host) return address.focus();
      const name = port.value === "45873" ? host : `${host}:${port.value}`;
      send(name, 94);
    });

    // the incoming transfer that is already on the page carries on once it's seen
    const incoming = box.children[1];
    if (incoming) {
      const t = transfer(incoming.querySelector("strong").textContent, { names: ["holiday-video.mp4"] }, incoming);
      t.status = "receiving";
      t.progress = 62;
      t.speed = 87.4;
      t.draw();
      if (calm) t.run("receiving", 87);
      else whenSeen(root, () => sleep(900).then(() => t.run("receiving", 87)));
    }

    // Links use example addresses, so the demo never connects to a local device.
    const links = $("[data-links]");
    const demoUrl = (fragment, scheme = "http") => `${scheme}://192.0.2.20:45873/#${fragment}=${LINK}`;
    const qrExamples = {
      d: { image: "qr-demo.svg", title: "downloadQr" },
      t: { image: "qr-text-demo.svg", title: "textQr" },
      r: { image: "qr-phone-demo.svg", title: "phoneQr" },
    };
    function showQr(fragment) {
      const { image, title } = qrExamples[fragment];
      const url = demoUrl(fragment);
      const img = el("img", "m-qr");
      img.src = new URL(image, ASSETS);
      img.alt = say("qrFor", { url });
      showDialog(say(title), el("p", "", say("demoAddress")), img, el("p", "m-qr-url", url));
    }
    $("[data-phone]").addEventListener("click", () => showQr("r"));

    function bindLink(item, fragment) {
      const [qrButton, stopButton] = item.querySelectorAll(".m-link-buttons button");
      qrButton.setAttribute("aria-haspopup", "dialog");
      qrButton.addEventListener("click", () => showQr(fragment));
      stopButton.addEventListener("click", () => item.remove());
    }
    for (const card of links.children) bindLink(card, "d");
    $("[data-create]").addEventListener("click", () => {
      const contents = payload();
      if (!contents) return;
      const fragment = contents.text === undefined ? "d" : "t";
      const item = el("div", "m-item enter");
      showContents(item, contents);
      const urls = el("div", "m-urls");
      urls.append(
        el("span", "url", demoUrl(fragment)),
        el("span", "hint", say("secure")),
        el("span", "url", demoUrl(fragment, "https")),
      );
      const buttons = el("div", "m-link-buttons");
      for (const label of ["qr", "stop"]) {
        const b = el("button", "m-btn sec", say(label));
        b.type = "button";
        buttons.append(b);
      }
      item.append(urls, buttons);
      bindLink(item, fragment);
      links.prepend(item);
    });
  }

  function terminal() {
    const screen = document.querySelector("[data-terminal]");
    if (!screen) return;
    const prompt = () => {
      const line = el("div");
      line.append(el("span", "prompt", "$ "));
      screen.append(line);
      return line;
    };
    const print = (item) => {
      if (item.qr) return screen.append(el("span", "qr", item.qr));
      if (item.url) return screen.append(el("div", "url", item.url));
      const last = item.progress && item.progress[item.progress.length - 1];
      screen.append(el("div", last ? "ok" : "out", last || item.out));
    };
    const bottom = () => {
      screen.scrollTop = screen.scrollHeight;
    };

    if (calm) {
      for (const item of SCRIPT) {
        if (item.cmd) prompt().append(el("span", "cmd", item.cmd));
        else print(item);
      }
      prompt().append(el("span", "caret"));
      return bottom();
    }

    async function run() {
      const caret = el("span", "caret");
      await sleep(500);
      for (const item of SCRIPT) {
        if (item.cmd) {
          const line = prompt();
          const cmd = el("span", "cmd");
          line.append(cmd, caret);
          bottom();
          await sleep(500);
          for (const ch of item.cmd) {
            cmd.textContent += ch;
            await sleep(ch === " " ? 40 : 35 + Math.random() * 45);
          }
          await sleep(350);
          caret.remove();
        } else if (item.progress) {
          const line = el("div", "out");
          screen.append(line);
          for (const [i, step] of item.progress.entries()) {
            line.textContent = step;
            if (i === item.progress.length - 1) line.className = "ok";
            bottom();
            await sleep(i === 0 ? 700 : 450);
          }
        } else {
          print(item);
          bottom();
          await sleep(item.qr ? 250 : 120);
        }
      }
      prompt().append(caret);
      bottom();
    }

    whenSeen(screen, run);
  }

  function whenSeen(node, run) {
    if (!("IntersectionObserver" in window)) return run();
    const seen = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          seen.disconnect();
          run();
        }
      },
      { threshold: 0.35 },
    );
    seen.observe(node);
  }

  function system() {
    const p = `${navigator.userAgentData?.platform || ""} ${navigator.platform || ""} ${navigator.userAgent}`;
    if (/android|iphone|ipad|ipod/i.test(p)) return "";
    if (/win/i.test(p)) return "windows";
    if (/mac/i.test(p)) return "mac";
    if (/linux|x11|cros/i.test(p)) return "linux";
    return "";
  }

  async function latestRelease() {
    try {
      const kept = sessionStorage.getItem("release");
      if (kept) return JSON.parse(kept);
    } catch {
      // no session storage: ask GitHub
    }
    const reply = await fetch(`https://api.github.com/repos/${REPO}/releases/latest`, {
      headers: { Accept: "application/vnd.github+json" },
    });
    if (!reply.ok) throw new Error(`GitHub said ${reply.status}`);
    const release = await reply.json();
    const kept = {
      tag: release.tag_name,
      url: release.html_url,
      date: release.published_at,
      files: Object.fromEntries(release.assets.map((a) => [a.name, a.browser_download_url])),
    };
    try {
      sessionStorage.setItem("release", JSON.stringify(kept));
    } catch {
      // fine, we'll ask again next time
    }
    return kept;
  }

  async function downloads() {
    const boxes = [...document.querySelectorAll("[data-downloads]")];
    if (!boxes.length) return;
    let release;
    try {
      release = await latestRelease();
    } catch {
      return; // the buttons already point at the releases page
    }
    for (const badge of document.querySelectorAll("[data-version]")) badge.textContent = release.tag;
    const file = (suffix) =>
      Object.entries(release.files).find(([name]) => name.endsWith(suffix))?.[1];
    const links = {
      linux: file("_linux_amd64.tar.gz"),
      windows: file("_windows_amd64.zip"),
      mac_arm64: file("_darwin_arm64.tar.gz"),
      mac_x64: file("_darwin_amd64.tar.gz"),
    };
    const os = system();
    const main = {
      linux: [["linux", TEXT.linux]],
      windows: [["windows", TEXT.windows]],
      mac: [
        ["mac_arm64", TEXT.mac_arm64],
        ["mac_x64", TEXT.mac_x64],
      ],
    }[os] || [
      ["linux", TEXT.linux_short],
      ["windows", TEXT.windows_short],
      ["mac_arm64", TEXT.mac_arm64],
      ["mac_x64", TEXT.mac_x64],
    ];
    // Ukrainian dates end in "р.". The sentence adds its own full stop, so drop this one.
    const when = new Date(release.date)
      .toLocaleDateString(lang === "uk" ? "uk-UA" : "en-GB", {
        year: "numeric",
        month: "long",
        day: "numeric",
      })
      .replace(/\.$/, "");
    for (const box of boxes) {
      const buttons = box.querySelector(".downloads");
      buttons.textContent = "";
      for (const [key, label] of main) {
        if (!links[key]) continue;
        const a = el("a", `button${os ? " primary" : ""}`, label);
        a.href = links[key];
        buttons.append(a);
      }
      const all = el("a", "button quiet", TEXT.all);
      all.href = release.url;
      buttons.append(all);
      const note = box.querySelector(".download-note");
      if (note) note.textContent = TEXT.version(release.tag.replace(/^v/, ""), when);
    }
  }

  for (const link of document.querySelectorAll("[data-lang]")) {
    link.addEventListener("click", () => remember("lang", link.dataset.lang));
  }

  app();
  terminal();
  downloads();
})();
