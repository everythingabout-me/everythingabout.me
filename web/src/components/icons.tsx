import type { SVGProps } from "react";

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function base({ size = 16, ...props }: IconProps) {
  return {
    width: size,
    height: size,
    viewBox: "0 0 16 16",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.5,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    ...props,
  };
}

export function FolderIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M1.75 4.25c0-.55.45-1 1-1h3.1c.27 0 .52.1.71.3l.94.94c.19.2.44.3.71.3h4.99c.55 0 1 .45 1 1v6.96c0 .55-.45 1-1 1H2.75c-.55 0-1-.45-1-1V4.25Z" />
    </svg>
  );
}

export function FileIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M3.5 2.25h5.19c.27 0 .52.1.71.3l2.8 2.79c.19.2.3.44.3.71v7.7c0 .55-.45 1-1 1h-8c-.55 0-1-.45-1-1V3.25c0-.55.45-1 1-1Z" />
      <path d="M8.5 2.5v2.75c0 .41.34.75.75.75h2.75" />
    </svg>
  );
}

export function ImageIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <rect x="2.25" y="3" width="11.5" height="10" rx="1.25" />
      <circle cx="5.75" cy="6.25" r="1" />
      <path d="m3 11.5 2.8-2.6c.3-.28.75-.28 1.05 0l2.15 2M9.4 10.2l1.2-1.1c.3-.28.76-.28 1.06 0l1.84 1.7" />
    </svg>
  );
}

export function VideoIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <rect x="2.25" y="3.75" width="8" height="8.5" rx="1.25" />
      <path d="m10.25 7.6 3.1-1.8c.27-.16.6.04.6.35v3.7c0 .31-.33.51-.6.35l-3.1-1.8V7.6Z" />
    </svg>
  );
}

export function AudioIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M6.25 11V4.5l5.5-1.25v6.5" />
      <circle cx="4.75" cy="11.25" r="1.5" />
      <circle cx="10.25" cy="9.75" r="1.5" />
    </svg>
  );
}

export function PdfIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M3.5 2.25h5.19c.27 0 .52.1.71.3l2.8 2.79c.19.2.3.44.3.71v7.7c0 .55-.45 1-1 1h-8c-.55 0-1-.45-1-1V3.25c0-.55.45-1 1-1Z" />
      <path d="M5.25 11.5V9M5.25 9h.9a.85.85 0 0 1 0 1.7h-.9M8 11.5V9h.75a1.25 1.25 0 0 1 0 2.5H8Z" />
    </svg>
  );
}

export function ArchiveIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <rect x="2.25" y="3" width="11.5" height="10" rx="1.25" />
      <path d="M2.25 6.25h11.5M7 8.5h2" />
    </svg>
  );
}

export function CodeIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="m6 5.5-3 2.75L6 11M10 5.5l3 2.75L10 11" />
    </svg>
  );
}

export function SearchIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <circle cx="7.25" cy="7.25" r="4" />
      <path d="m10.4 10.4 2.6 2.6" />
    </svg>
  );
}

export function UploadIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M8 10.5V2.75M5.25 5.5 8 2.75l2.75 2.75M2.75 10.5v1.75c0 .55.45 1 1 1h8.5c.55 0 1-.45 1-1V10.5" />
    </svg>
  );
}

export function PlusIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M8 3.25v9.5M3.25 8h9.5" />
    </svg>
  );
}

export function RefreshIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M13.25 8a5.25 5.25 0 1 1-1.6-3.78" />
      <path d="M13.5 2.75v3.1h-3.1" />
    </svg>
  );
}

export function DownloadIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M8 2.75v7.75M5.25 7.75 8 10.5l2.75-2.75M2.75 11.75v.5c0 .55.45 1 1 1h8.5c.55 0 1-.45 1-1v-.5" />
    </svg>
  );
}

export function TrashIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M2.75 4.25h10.5M6.25 4.25V3c0-.41.34-.75.75-.75h2c.41 0 .75.34.75.75v1.25M4 4.25l.6 8.55c.03.4.36.7.75.7h5.3c.39 0 .72-.3.75-.7L12 4.25" />
    </svg>
  );
}

export function CloseIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="m4 4 8 8M12 4l-8 8" />
    </svg>
  );
}

export function ChevronRightIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="m6 3.5 4.5 4.5L6 12.5" />
    </svg>
  );
}

export function ChevronDownIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="m3.5 6 4.5 4.5L12.5 6" />
    </svg>
  );
}

export function PencilIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="M10.4 2.6a1.55 1.55 0 0 1 2.2 2.2L5.6 11.8l-3 .8.8-3z" />
      <path d="m9.3 3.7 2.2 2.2" />
    </svg>
  );
}

/** A window: an app connection. */
export function AppIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <rect x="2.25" y="3" width="11.5" height="10" rx="1.5" />
      <path d="M2.25 6h11.5" />
      <path d="M4.5 4.5h.01M6 4.5h.01" />
    </svg>
  );
}

/** A key: a temporary token. */
export function KeyIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <circle cx="5.5" cy="5.5" r="2.5" />
      <path d="m7.3 7.3 6.2 6.2" />
      <path d="m10.6 10.6 1.5-1.5" />
      <path d="m12.2 12.2 1.2-1.2" />
    </svg>
  );
}

export function CopyIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <rect x="5.25" y="5.25" width="8" height="8.5" rx="1.5" />
      <path d="M10.75 5.25v-1.5c0-.55-.45-1-1-1h-6c-.55 0-1 .45-1 1v7c0 .55.45 1 1 1h1.5" />
    </svg>
  );
}

export function CheckIcon(props: IconProps) {
  return (
    <svg {...base(props)}>
      <path d="m3.75 8.25 2.75 2.75 5.75-5.75" />
    </svg>
  );
}

/** Pick an icon by extension, keeping the visuals restrained */
export function GlyphFor({ name, mime, isDir }: { name: string; mime?: string | null; isDir: boolean }) {
  if (isDir) return <FolderIcon />;
  const ext = name.split(".").pop()?.toLowerCase() ?? "";
  const type = mime ?? "";
  if (type.startsWith("image/") || ["png", "jpg", "jpeg", "gif", "webp", "heic", "avif"].includes(ext))
    return <ImageIcon />;
  if (type.startsWith("video/") || ["mp4", "mov", "mkv", "webm"].includes(ext)) return <VideoIcon />;
  if (type.startsWith("audio/") || ["mp3", "flac", "wav", "m4a"].includes(ext)) return <AudioIcon />;
  if (ext === "pdf") return <PdfIcon />;
  if (["zip", "rar", "7z", "tar", "gz"].includes(ext)) return <ArchiveIcon />;
  if (["ts", "tsx", "js", "jsx", "go", "py", "rs", "json", "md", "yml", "yaml", "sh"].includes(ext))
    return <CodeIcon />;
  return <FileIcon />;
}
