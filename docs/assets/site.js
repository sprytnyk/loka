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
      nothing: "Nothing selected",
      first: "Choose files or directories first.",
      remove: "Remove",
      to: "To {name}",
      fromBrowser: "From a browser at {name}",
      statuses: { waiting: "waiting", sending: "sending", receiving: "receiving", complete: "complete", cancelled: "cancelled" },
      speed: "{speed} Mbps",
      cancel: "Cancel",
      code: "Pairing code {code}. Check that the other device shows the same.",
      secure: "Safer, once the phone has Loka's certificate:",
      qr: "QR code",
      qrFor: "QR code for {url}",
      stop: "Stop sharing",
    },
    uk: {
      selected: { one: "Вибрано {n} елемент", few: "Вибрано {n} елементи", many: "Вибрано {n} елементів", other: "Вибрано {n} елемента" },
      names: { one: "{n} назва", few: "{n} назви", many: "{n} назв", other: "{n} назви" },
      nothing: "Нічого не вибрано",
      first: "Спершу виберіть файли або теки.",
      remove: "Прибрати",
      to: "До {name}",
      fromBrowser: "Від браузера за адресою {name}",
      statuses: { waiting: "очікує", sending: "надсилання", receiving: "отримання", complete: "завершено", cancelled: "скасовано" },
      speed: "{speed} Мбіт/с",
      cancel: "Скасувати",
      code: "Код з’єднання {code}. Перевірте, що інший пристрій показує такий самий.",
      secure: "Безпечніше, коли на телефоні вже є сертифікат Loka:",
      qr: "QR-код",
      qrFor: "QR-код для {url}",
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

    // Items to share
    const list = $("[data-selected]");
    const count = $("[data-count]");
    const more = [
      "/home/alex/Music/live-set.flac",
      "/home/alex/Projects/website",
      "/home/alex/Downloads/manual.pdf",
      "/home/alex/Pictures/weekend-photos",
      "/home/alex/Documents/itinerary.pdf",
    ];
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
      b.addEventListener("click", () => add(more.find((p) => !selected().includes(p))));
    }
    $("[data-add]").addEventListener("submit", (e) => {
      e.preventDefault();
      const input = e.currentTarget.querySelector("input");
      add(input.value.trim());
      input.value = "";
    });
    // like Loka, it asks for something to send first
    const items = () => {
      const paths = selected();
      if (!paths.length) {
        count.textContent = say("first");
        count.classList.add("warn");
      }
      return paths.map(base);
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

    function transfer(title, names, card) {
      const t = { status: "waiting", progress: 0, speed: null, stopped: false };
      const item = card || el("div", "m-item enter");
      item.textContent = "";
      const row = el("div", "m-row");
      const status = el("span", "m-status");
      row.append(el("strong", "", title), status);
      item.append(row);
      if (names.length === 1) {
        const p = el("p");
        p.append(el("small", "", names[0]));
        item.append(p);
      } else if (names.length) {
        const files = el("details", "m-files");
        files.append(el("summary", "", say("names", { n: names.length })));
        const ul = el("ul");
        for (const name of names) ul.append(el("li", "", name));
        files.append(ul);
        item.append(files);
      }
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
      const names = items();
      if (!names.length) return;
      const t = transfer(say("to", { name }), names);
      t.code = String(100000 + Math.floor(Math.random() * 900000)).replace(/(\d{3})(\d{3})/, "$1 $2");
      t.draw();
      await sleep(calm ? 0 : 1800);
      if (!t.stopped) t.run("sending", mbps);
    }
    for (const b of root.querySelectorAll("[data-send]")) {
      b.addEventListener("click", () => send(b.dataset.send, Number(b.dataset.speed)));
    }
    $("[data-manual]").addEventListener("submit", (e) => {
      e.preventDefault();
      const address = e.currentTarget.querySelector("input").value.trim() || "192.0.2.30";
      send(address, 94);
    });
    // a photo from a phone's browser, which Loka would ask to accept first
    $("[data-phone]").addEventListener("click", () => {
      const t = transfer(say("fromBrowser", { name: "192.0.2.40" }), ["IMG_2041.HEIC"]);
      sleep(calm ? 0 : 700).then(() => t.run("receiving", 64));
    });

    // the incoming transfer that is already on the page carries on once it's seen
    const incoming = box.children[1];
    if (incoming) {
      const t = transfer(incoming.querySelector("strong").textContent, ["holiday-video.mp4"], incoming);
      t.status = "receiving";
      t.progress = 62;
      t.speed = 87.4;
      t.draw();
      if (calm) t.run("receiving", 87);
      else whenSeen(root, () => sleep(900).then(() => t.run("receiving", 87)));
    }

    // Download links
    const links = $("[data-links]");
    const URL_TAIL = `192.0.2.20:45873/#d=${LINK}`;
    function link(names, card) {
      const item = card || el("div", "m-item enter");
      if (!card) {
        item.append(el("small", "", names.join(", ")));
        const urls = el("div", "m-urls");
        urls.append(
          el("span", "url", `http://${URL_TAIL}`),
          el("span", "hint", say("secure")),
          el("span", "url", `https://${URL_TAIL}`),
        );
        const buttons = el("div", "m-link-buttons");
        buttons.append(el("button", "m-btn sec", say("qr")), el("button", "m-btn sec", say("stop")));
        for (const b of buttons.children) b.type = "button";
        item.append(urls, buttons);
        links.prepend(item);
      }
      const [qrButton, stopButton] = item.querySelectorAll(".m-link-buttons button");
      qrButton.addEventListener("click", () => {
        const shown = item.querySelector(".m-qr");
        if (shown) return shown.remove();
        const img = el("img", "m-qr");
        img.src = new URL("qr-demo.svg", ASSETS);
        img.alt = say("qrFor", { url: `http://${URL_TAIL}` });
        item.querySelector(".m-urls").after(img);
      });
      stopButton.addEventListener("click", () => item.remove());
    }
    for (const card of links.children) link([], card);
    $("[data-create]").addEventListener("click", () => {
      const names = items();
      if (names.length) link(names);
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
