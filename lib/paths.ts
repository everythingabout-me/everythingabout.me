/** 可见文件统一放在 drive/ 前缀下，其余前缀是系统私有区 */
export const DRIVE_PREFIX = "drive/";
export const TRASH_PREFIX = "_trash/";
export const INVENTORY_PREFIX = "_inventory/";

export const MAX_PATH_DEPTH = 32;
export const MAX_NAME_LENGTH = 200;

export class PathError extends Error {}

/**
 * 归一化为 NFC、去掉首尾斜杠、折叠重复斜杠。
 * macOS 上传的文件名是 NFD，不归一化会和 Linux 侧产生"同名两个文件"。
 */
export function normalizePath(input: string | null | undefined): string {
  let value = (input ?? "").normalize("NFC").replace(/\\/g, "/").trim();
  const segments: string[] = [];
  for (const segment of value.split("/")) {
    if (!segment || segment === ".") continue;
    if (segment === "..") throw new PathError("路径不能包含 ..");
    if (segment.length > MAX_NAME_LENGTH) throw new PathError("单级名称过长");
    if (/[\u0000-\u001f\u007f]/.test(segment)) throw new PathError("路径包含控制字符");
    segments.push(segment);
  }
  if (segments.length > MAX_PATH_DEPTH) throw new PathError("目录层级过深");
  value = segments.join("/");
  return value;
}

export function parentOf(path: string): string {
  const index = path.lastIndexOf("/");
  return index === -1 ? "" : path.slice(0, index);
}

export function nameOf(path: string): string {
  const index = path.lastIndexOf("/");
  return index === -1 ? path : path.slice(index + 1);
}

export function isInside(path: string, ancestor: string): boolean {
  if (!ancestor) return true;
  return path === ancestor || path.startsWith(ancestor + "/");
}

/** 文件对象 key */
export function keyFor(path: string): string {
  return DRIVE_PREFIX + path;
}

/** 目录标记对象 key（以 / 结尾，方便 coscli / rclone 识别空目录） */
export function dirKeyFor(path: string): string {
  return DRIVE_PREFIX + (path ? path + "/" : "");
}

/** 从对象 key 还原逻辑路径；不属于 drive/ 的返回 null */
export function pathFromKey(key: string): string | null {
  if (!key.startsWith(DRIVE_PREFIX)) return null;
  const rest = key.slice(DRIVE_PREFIX.length).replace(/\/+$/, "");
  return rest;
}

export function ancestorsOf(path: string): string[] {
  const result: string[] = [];
  let current = parentOf(path);
  while (current) {
    result.push(current);
    current = parentOf(current);
  }
  return result.reverse();
}
