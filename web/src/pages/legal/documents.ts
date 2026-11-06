/**
 * The privacy policy and the terms of service.
 *
 * These are long-form documents rather than interface strings, so they live here instead of
 * i18n/messages.ts; the chrome around them (page titles, "last updated", back links) is still
 * UI copy and stays in messages.ts. Both languages are written in full: a legal page that
 * silently falls back to English is not acceptable to the people relying on it.
 */
import { locale, type Locale } from "../../i18n";

export type LegalSection = {
  heading: string;
  paragraphs: string[];
  bullets?: string[];
};

export type LegalDocument = {
  updated: string;
  intro: string[];
  sections: LegalSection[];
};

export type LegalDocumentKey = "privacy" | "terms";

const en: Record<LegalDocumentKey, LegalDocument> = {
  privacy: {
    updated: "November 6, 2026",
    intro: [
      "Everything About Me is a personal data service at everythingabout.me: one account, one file layer, and one automatically organized stream of what you put in it. It is run by a single person, not a company.",
      "This page explains what the service stores, why it stores it, where it lives, and what you can do about it. It covers the website and the API on the same domain.",
    ],
    sections: [
      {
        heading: "Information we store",
        paragraphs: [
          "An account needs an email address. We also store an optional password as a salted scrypt hash — never the password itself — the six-digit sign-in codes we email you, and session records containing the browser's user agent, IP address, creation time and expiry.",
        ],
        bullets: [
          "Content: the files and folders you upload, with their names, paths, sizes, types, timestamps and checksums, plus previews and metadata derived from them.",
          "Connections: the API tokens and app connections you create — a hash of the token, the name you give it, its scopes, its expiry and when it was last used.",
          "History: a record of the changes made through the service (upload, move, copy, rename, delete, download, connection changes), so the activity log and the file audit are real.",
          "Logs: request paths, status codes, timings and source IP addresses, kept briefly to operate and secure the service.",
        ],
      },
      {
        heading: "Cookies",
        paragraphs: [
          "One session cookie keeps you signed in. It is HttpOnly and, on HTTPS, Secure; scripts cannot read it and it is not shared with anyone. Requests that change data from a browser also carry a CSRF token, which is not a cookie and is not stored by us beyond your session.",
          "There is no analytics script, advertising pixel or third-party tracker anywhere in the app.",
        ],
      },
      {
        heading: "How we use it",
        paragraphs: [
          "To sign you in, store and serve your files, build the timeline and search index from your own content, send sign-in codes, keep the service secure and working, and answer you when you ask for help. Organizational work — grouping, scoring and narrating your stream — runs on our own server with local rules; your files are not sent to a third-party AI provider.",
          "We do not sell your data, do not use it for advertising, do not build profiles to share with anyone, and do not read your files except where you ask us to, or where it is necessary to fix a problem you reported.",
        ],
      },
      {
        heading: "Where your data lives",
        paragraphs: [
          "The database and application run on a server in Hong Kong. File bytes live in Tencent Cloud Object Storage (COS) in Hong Kong, sign-in email is delivered through Tencent Cloud SES, and the site is served through Tencent EdgeOne. These providers process data on our behalf, only for those specific tasks, under their own terms. If you are outside Hong Kong, your data is transferred there in order to provide the service.",
        ],
      },
      {
        heading: "Third-party accounts",
        paragraphs: [
          "The service does not require a Google, Apple or any other third-party account. If you connect an external service such as a cloud drive in the future, we will ask only for the scopes that connection needs, act only on the files and folders you choose, use the data solely to do what you asked, and share it with no one else.",
          "Data obtained from Google APIs is never used for advertising, never sold, and never used to train or improve generalized AI models; it is transferred only as needed to provide the feature you requested or as required by law.",
        ],
      },
      {
        heading: "How long we keep it",
        paragraphs: [
          "Files stay until you delete them. Deletion is permanent: there is no trash and no undo. Sign-in codes expire in ten minutes, sessions expire after 30 days, and tokens expire when you set them to. Database backups are kept for seven days, and server logs are short-lived. Account records remain until the account is deleted.",
        ],
      },
      {
        heading: "Security",
        paragraphs: [
          "Traffic is encrypted with TLS. Passwords and tokens are stored hashed. Access to object storage uses short-lived, narrowly scoped credentials. The server is reachable only by the person who runs it. No service can promise perfect security; if a breach ever affects your data, we will tell you what happened and what to do.",
        ],
      },
      {
        heading: "Your choices",
        paragraphs: [
          "You can browse, download and delete anything you store, change your email address, and revoke any connection at any time in the app. To delete your entire account and everything in it, write to the address below; we will do it within 30 days, except for records we must keep by law. You can also ask what we hold about you and receive a copy.",
        ],
      },
      {
        heading: "Children",
        paragraphs: [
          "The service is not intended for children under 13, or under the minimum age where you live, and we do not knowingly store their data.",
        ],
      },
      {
        heading: "Changes",
        paragraphs: [
          "When this policy changes, the date at the top changes with it. Material changes will be announced in the app before they take effect.",
        ],
      },
      {
        heading: "Contact",
        paragraphs: ["Li Zhao — iloahz@gmail.com. The same address runs the server."],
      },
    ],
  },
  terms: {
    updated: "November 6, 2026",
    intro: [
      "These terms are the agreement between you and the person who runs Everything About Me (Li Zhao). By creating an account or using the service, you accept them. If you do not accept them, please do not use the service.",
      "The service is a personal project, not a company. It is offered as it is, and these terms are meant to be read rather than to hide anything.",
    ],
    sections: [
      {
        heading: "The service",
        paragraphs: [
          "Everything About Me gives you an email-backed account, a file layer with folders and metadata, and a stream that organizes what you store. Features may be added, changed or removed over time.",
        ],
      },
      {
        heading: "Your account",
        paragraphs: [
          "You need an email address you control. Keep your password, tokens and app connections private: anything done with them counts as done by you. Tell us immediately if you believe someone else has access. Do not create accounts to abuse the service or to evade its limits.",
        ],
      },
      {
        heading: "Your content",
        paragraphs: [
          "Everything you upload remains yours. You give us only the permission needed to store it, copy it between folders, generate previews, index it, and send it back to you or to an app you have authorized — nothing more. You confirm that you have the right to upload it and that it does not violate anyone else's rights.",
        ],
      },
      {
        heading: "Acceptable use",
        paragraphs: ["You agree not to:"],
        bullets: [
          "store illegal content, malware, or material that infringes someone else's rights;",
          "attack, scan or overload the service, or attempt to reach other people's data or accounts;",
          "resell the service or use it as public file hosting or a CDN for third parties;",
          "circumvent quotas, rate limits or access controls.",
        ],
      },
      {
        heading: "Connections and the API",
        paragraphs: [
          "Tokens and app connections act as you, within the scope you grant them. What an app does with a token is your responsibility; revoke anything you no longer trust. A token is shown only once, so store it safely.",
        ],
      },
      {
        heading: "Availability",
        paragraphs: [
          "The service is run by one person. It may be offline for maintenance, may change, and may stop. There is no uptime guarantee and no backup guarantee: keep your own copy of anything you cannot afford to lose. Deletion is permanent by design.",
        ],
      },
      {
        heading: "No warranty",
        paragraphs: [
          "To the extent permitted by law, the service is provided \u201cas is\u201d and \u201cas available\u201d, without warranties of any kind, express or implied, including merchantability, fitness for a particular purpose and non-infringement.",
        ],
      },
      {
        heading: "Limitation of liability",
        paragraphs: [
          "To the extent permitted by law, the operator is not liable for indirect, incidental, special or consequential damages, or for lost, corrupted or unavailable data, arising from your use of, or inability to use, the service. Nothing here limits liability that cannot be limited by law, including liability for intentional misconduct or gross negligence.",
        ],
      },
      {
        heading: "Ending it",
        paragraphs: [
          "You can stop using the service at any time and delete your content. We may suspend or end access if these terms are broken, if the service is discontinued, or if keeping the account would put the service or others at risk. Export what you need before you go.",
        ],
      },
      {
        heading: "Changes to these terms",
        paragraphs: [
          "Updated terms are posted here with a new date. Continuing to use the service after a change means you accept the new version.",
        ],
      },
      {
        heading: "Law and disputes",
        paragraphs: [
          "These terms are governed by the laws applicable where the operator lives, and disputes are subject to the courts there, without regard to conflict-of-law rules. If you use the service as a consumer, you keep any mandatory rights your local law gives you.",
        ],
      },
      {
        heading: "Contact",
        paragraphs: ["Li Zhao — iloahz@gmail.com."],
      },
    ],
  },
};

const zh: Record<LegalDocumentKey, LegalDocument> = {
  privacy: {
    updated: "2026 年 11 月 6 日",
    intro: [
      "Everything About Me 是 everythingabout.me 上的一个个人数据服务：一个账号、一层文件，以及一条自动整理你所存内容的流。它由个人维护，背后没有公司。",
      "本页说明这个服务保存什么、为什么保存、数据放在哪里，以及你可以怎么处理。它同时适用于同一域名下的网站和 API。",
    ],
    sections: [
      {
        heading: "我们保存的信息",
        paragraphs: [
          "账号需要一个邮箱地址。我们还会保存可选密码的加盐 scrypt 哈希（从不保存密码本身）、发给你的六位登录验证码，以及包含浏览器 user agent、IP 地址、创建时间和过期时间的会话记录。",
        ],
        bullets: [
          "内容：你上传的文件和文件夹，及其名称、路径、大小、类型、时间戳和校验和，还有由它们生成的预览和元数据。",
          "连接：你创建的 API 令牌和应用连接——令牌的哈希、你给它的名字、权限范围、有效期和最近使用时间。",
          "历史：通过这个服务产生的变更记录（上传、移动、复制、重命名、删除、下载、连接变更），活动日志和文件审计就来自它。",
          "日志：请求路径、状态码、耗时和来源 IP，短期保留，用于运行和安全。",
        ],
      },
      {
        heading: "Cookie",
        paragraphs: [
          "只有一个会话 Cookie 用来保持登录。它是 HttpOnly 的，在 HTTPS 下带 Secure；脚本读不到它，也不会和任何人共享。浏览器发起的写操作还会带一个 CSRF 令牌，它不是 Cookie，除了你的会话期间我们不会另行保存。",
          "应用里没有任何统计脚本、广告像素或第三方追踪。",
        ],
      },
      {
        heading: "我们如何使用",
        paragraphs: [
          "用于登录、保存和取回文件、根据你自己的内容生成时间线和搜索索引、发送验证码、保持服务安全和可用，以及在你求助时回复你。整理工作——分组、打分、叙述——在自有服务器上用本地规则完成；你的文件不会发给第三方 AI 服务商。",
          "我们不出售你的数据，不用于广告，不为任何其他人建立画像；除了你主动请求，或为了修复你报告的问题所必需，我们不读取你的文件。",
        ],
      },
      {
        heading: "数据放在哪里",
        paragraphs: [
          "数据库和应用运行在香港的一台服务器上。文件本体存放在香港的腾讯云对象存储（COS），登录邮件通过腾讯云 SES 发送，站点通过腾讯云 EdgeOne 提供访问。这些服务商只为我们完成上述具体任务，并受其自身条款约束。如果你不在香港，数据会被传输到香港以提供服务。",
        ],
      },
      {
        heading: "第三方账号",
        paragraphs: [
          "这个服务不需要 Google、Apple 或任何第三方账号。如果将来你连接云盘之类的外部服务，我们只会申请该连接所需的最小权限，只操作你选定的文件和文件夹，只把你要求的事情做完，不会分享给其他人。",
          "从 Google API 获得的数据绝不用于广告、绝不出售、绝不用于训练或改进通用 AI 模型；只在提供你请求的功能所必需或法律要求时传输。",
        ],
      },
      {
        heading: "保留多久",
        paragraphs: [
          "文件会一直保留到你删除。删除是永久的：没有回收站，也无法撤销。验证码 10 分钟过期，会话 30 天过期，令牌按你设定的时间过期。数据库备份保留 7 天，服务器日志短期保留。账号记录会一直保留到账号被删除。",
        ],
      },
      {
        heading: "安全",
        paragraphs: [
          "流量用 TLS 加密，密码和令牌只保存哈希，访问对象存储使用短期、范围受限的凭证。服务器只有维护者本人能够访问。没有服务能承诺绝对安全；如果发生影响你数据的安全事件，我们会告诉你发生了什么、该怎么做。",
        ],
      },
      {
        heading: "你可以做什么",
        paragraphs: [
          "你可以随时在应用里浏览、下载、删除任何内容，修改邮箱，撤销任何连接。要删除整个账号和其中所有内容，请写信给下面的地址，我们会在 30 天内完成（法律要求保留的记录除外）。你也可以询问我们持有哪些关于你的数据，并索取一份副本。",
        ],
      },
      {
        heading: "儿童",
        paragraphs: [
          "本服务不面向 13 岁以下（或你所在地区规定的最低年龄）的儿童，我们不会有意保存他们的数据。",
        ],
      },
      {
        heading: "变更",
        paragraphs: ["本政策变更时，顶部的日期会随之更新。重大变更会在生效前于应用内说明。"],
      },
      {
        heading: "联系",
        paragraphs: ["Li Zhao — iloahz@gmail.com。运行这台服务器的也是这个地址。"],
      },
    ],
  },
  terms: {
    updated: "2026 年 11 月 6 日",
    intro: [
      "本条款是你与 Everything About Me 维护者（Li Zhao）之间的约定。创建账号或使用本服务，即表示你接受这些条款；如果不接受，请不要使用本服务。",
      "这是一个个人项目，背后没有公司。它按现状提供，这份条款希望写得能看懂，而不是为了藏东西。",
    ],
    sections: [
      {
        heading: "服务内容",
        paragraphs: [
          "Everything About Me 提供一个以邮箱为基础的账号、一层带文件夹和元数据的文件存储，以及一条自动整理你所存内容的流。功能可能随时间增加、变化或移除。",
        ],
      },
      {
        heading: "你的账号",
        paragraphs: [
          "你需要一个你能控制的邮箱地址。请保管好密码、令牌和应用连接：用它们做出的操作都算你本人做的。如果你认为别人已经拿到访问权限，请立刻告诉我们。不要为了滥用服务或绕过限制而创建账号。",
        ],
      },
      {
        heading: "你的内容",
        paragraphs: [
          "你上传的一切仍然属于你。你只授予我们保存它、在文件夹之间复制、生成预览、建立索引，以及把它还给你或你授权的应用所必需的权限——仅此而已。你确认自己有权上传这些内容，且不会侵犯他人的权利。",
        ],
      },
      {
        heading: "可接受的使用",
        paragraphs: ["你同意不做以下事情："],
        bullets: [
          "存放违法内容、恶意软件，或侵犯他人权利的材料；",
          "攻击、扫描或过载本服务，或试图访问他人的数据或账号；",
          "转售本服务，或把它当作面向第三方的公共文件托管或 CDN；",
          "绕过配额、速率限制或访问控制。",
        ],
      },
      {
        heading: "连接与 API",
        paragraphs: [
          "令牌和应用连接会以你授予的权限代表你行事。应用用令牌做了什么由你负责；不再信任的连接请及时撤销。令牌只会显示一次，请妥善保存。",
        ],
      },
      {
        heading: "可用性",
        paragraphs: [
          "这项服务由一个人维护。它可能因维护而离线，可能变化，也可能停止。我们不承诺可用性，也不承诺备份：无法承受丢失的东西请自己另存一份。按设计，删除是永久的。",
        ],
      },
      {
        heading: "免责",
        paragraphs: [
          "在法律允许的范围内，本服务按“现状”和“现有”提供，不作任何明示或默示的担保，包括适销性、特定用途适用性和不侵权。",
        ],
      },
      {
        heading: "责任限制",
        paragraphs: [
          "在法律允许的范围内，对于因使用或无法使用本服务而产生的间接、附带、特殊或后果性损害，以及数据丢失、损坏或不可用，维护者不承担责任。本条不限制法律不允许限制的责任，包括故意或重大过失。",
        ],
      },
      {
        heading: "结束使用",
        paragraphs: [
          "你可以随时停止使用并删除你的内容。如果违反条款、服务停止，或继续保留账号会给服务或他人带来风险，我们可能暂停或终止访问。离开前请先导出需要的东西。",
        ],
      },
      {
        heading: "条款变更",
        paragraphs: [
          "更新后的条款会连同新日期发布在这里。变更后继续使用本服务，即表示接受新版本。",
        ],
      },
      {
        heading: "法律与争议",
        paragraphs: [
          "本条款受维护者所在地适用的法律管辖，争议由当地法院处理，不适用冲突法规则。如果你以消费者身份使用本服务，当地法律赋予你的强制性权利不受影响。",
        ],
      },
      {
        heading: "联系",
        paragraphs: ["Li Zhao — iloahz@gmail.com。"],
      },
    ],
  },
};

const documents: Record<Locale, Record<LegalDocumentKey, LegalDocument>> = { en, zh };

export function legalDocument(key: LegalDocumentKey): LegalDocument {
  return documents[locale][key];
}
