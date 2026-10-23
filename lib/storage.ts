export interface ObjectInfo {
  key: string;
  size: number;
  etag?: string;
  lastModified?: Date;
  storageClass?: string;
}

export interface ListPage {
  objects: ObjectInfo[];
  nextMarker?: string;
}

export interface SignedUrlOptions {
  expiresSeconds?: number;
  downloadName?: string;
  contentType?: string;
}

export interface Storage {
  /** 浏览器直传用的预签名 PUT */
  putUrl(key: string, options?: SignedUrlOptions): Promise<string>;
  /** 下载用的预签名 GET，默认 inline */
  getUrl(key: string, options?: SignedUrlOptions): Promise<string>;
  head(key: string): Promise<ObjectInfo | null>;
  /** 服务端复制（不经过本地带宽） */
  copy(sourceKey: string, targetKey: string): Promise<void>;
  remove(key: string): Promise<void>;
  /** 分页列举，回调返回 false 可提前结束 */
  list(prefix: string, onPage: (page: ListPage) => Promise<boolean | void>): Promise<void>;
  /** 零字节对象，用于目录占位 */
  putEmpty(key: string): Promise<void>;
  /** 生成前端直传所需的临时凭证 */
  temporaryCredentials(
    key: string,
    expiresSeconds?: number,
  ): Promise<{
    tmpSecretId: string;
    tmpSecretKey: string;
    sessionToken: string;
    startTime: number;
    expiredTime: number;
  }>;
}
