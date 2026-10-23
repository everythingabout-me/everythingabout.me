import COS from "cos-nodejs-sdk-v5";
import STS from "qcloud-cos-sts";
import { cosAppId, env } from "./env";
import type { ListPage, ObjectInfo, SignedUrlOptions, Storage } from "./storage";

/** 把回调风格 SDK 包成 Promise */
function call<T>(executor: (cb: (err: unknown, data: T) => void) => void): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    executor((err, data) => (err ? reject(err) : resolve(data)));
  });
}

function encodeKey(key: string): string {
  return key.split("/").map(encodeURIComponent).join("/");
}

type HeadResult = Record<string, string | number | undefined>;

function headField(head: HeadResult, ...names: string[]): string | undefined {
  for (const name of names) {
    const direct = head[name];
    if (direct !== undefined && direct !== null) return String(direct);
    const lower = head[name.toLowerCase()];
    if (lower !== undefined && lower !== null) return String(lower);
  }
  return undefined;
}

export class CosStorage implements Storage {
  private readonly client: any;
  private readonly bucket: string;
  private readonly region: string;

  constructor() {
    const cfg = env.cos;
    this.bucket = cfg.bucket;
    this.region = cfg.region;
    this.client = new (COS as any)({ SecretId: cfg.secretId, SecretKey: cfg.secretKey });
  }

  private base() {
    return { Bucket: this.bucket, Region: this.region };
  }

  async putUrl(key: string, options: SignedUrlOptions = {}): Promise<string> {
    const data = await call<{ Url: string }>((cb) =>
      this.client.getObjectUrl(
        {
          ...this.base(),
          Key: key,
          Method: "PUT",
          Sign: true,
          Expires: options.expiresSeconds ?? 3600,
        },
        cb,
      ),
    );
    return data.Url;
  }

  async getUrl(key: string, options: SignedUrlOptions = {}): Promise<string> {
    const query: Record<string, string> = {};
    const name = options.downloadName;
    if (name) {
      query["response-content-disposition"] = `inline; filename*=UTF-8''${encodeURIComponent(name)}`;
    }
    if (options.contentType) {
      query["response-content-type"] = options.contentType;
    }
    const data = await call<{ Url: string }>((cb) =>
      this.client.getObjectUrl(
        {
          ...this.base(),
          Key: key,
          Method: "GET",
          Sign: true,
          Expires: options.expiresSeconds ?? 900,
          Query: Object.keys(query).length ? query : undefined,
        },
        cb,
      ),
    );
    return data.Url;
  }

  async head(key: string): Promise<ObjectInfo | null> {
    try {
      // Headers 必须传对象：SDK 在错误分支里会直接读 params.Headers
      const result = await call<HeadResult & { headers?: Record<string, string> }>((cb) =>
        this.client.headObject({ ...this.base(), Key: key, Headers: {} }, cb),
      );
      const headers = (result.headers ?? {}) as Record<string, string>;
      const lookup = (name: string) => headers[name] ?? headers[name.toLowerCase()];
      const modified = lookup("last-modified") ?? headField(result, "Last-Modified");
      return {
        key,
        size: Number(lookup("content-length") ?? headField(result, "Content-Length") ?? 0),
        etag: (lookup("etag") ?? headField(result, "ETag"))?.replace(/"/g, ""),
        lastModified: modified ? new Date(modified) : undefined,
        storageClass: lookup("x-cos-storage-class") ?? lookup("x-cos-storage-tier"),
      };
    } catch (error) {
      const status = (error as { statusCode?: number }).statusCode;
      if (status === 404) return null;
      throw error;
    }
  }

  async copy(sourceKey: string, targetKey: string): Promise<void> {
    await call((cb) =>
      this.client.putObjectCopy(
        {
          ...this.base(),
          Key: targetKey,
          CopySource: `${this.bucket}.cos.${this.region}.myqcloud.com/${encodeKey(sourceKey)}`,
        },
        cb,
      ),
    );
  }

  async remove(key: string): Promise<void> {
    await call((cb) => this.client.deleteObject({ ...this.base(), Key: key }, cb));
  }

  async putEmpty(key: string): Promise<void> {
    await call((cb) =>
      this.client.putObject({ ...this.base(), Key: key, Body: Buffer.alloc(0) }, cb),
    );
  }

  async list(
    prefix: string,
    onPage: (page: ListPage) => Promise<boolean | void>,
  ): Promise<void> {
    let marker: string | undefined;
    for (;;) {
      const data = await call<{
        Contents?: Array<{
          Key: string;
          Size: string;
          ETag?: string;
          LastModified?: string;
          StorageClass?: string;
        }>;
        IsTruncated: string | boolean;
        NextMarker?: string;
      }>((cb) =>
        this.client.getBucket(
          {
            ...this.base(),
            Prefix: prefix,
            Marker: marker,
            MaxKeys: 1000,
          },
          cb,
        ),
      );

      const objects: ObjectInfo[] = (data.Contents ?? []).map((item) => ({
        key: item.Key,
        size: Number(item.Size ?? 0),
        etag: item.ETag?.replace(/"/g, ""),
        lastModified: item.LastModified ? new Date(item.LastModified) : undefined,
        storageClass: item.StorageClass,
      }));

      const truncated =
        data.IsTruncated === true || String(data.IsTruncated).toLowerCase() === "true";
      const proceed = await onPage({ objects, nextMarker: data.NextMarker });
      if (proceed === false) return;
      if (!truncated || !data.NextMarker) return;
      marker = data.NextMarker;
    }
  }

  async temporaryCredentials(key: string, expiresSeconds = 1800) {
    const cfg = env.cos;
    const appId = cosAppId(cfg.bucket);
    const resource = `qcs::cos:${cfg.region}:uid/${appId}:${cfg.bucket}/${key}`;
    const policy = {
      version: "2.0",
      statement: [
        {
          effect: "allow",
          action: [
            "name/cos:PutObject",
            "name/cos:PostObject",
            "name/cos:InitiateMultipartUpload",
            "name/cos:UploadPart",
            "name/cos:CompleteMultipartUpload",
            "name/cos:AbortMultipartUpload",
            "name/cos:ListMultipartUploads",
            "name/cos:ListParts",
          ],
          resource: [resource],
        },
      ],
    };

    const data = await new Promise<{
      credentials: { tmpSecretId: string; tmpSecretKey: string; sessionToken: string };
      startTime: number;
      expiredTime: number;
    }>((resolve, reject) => {
      STS.getCredential(
        {
          secretId: cfg.secretId,
          secretKey: cfg.secretKey,
          durationSeconds: expiresSeconds,
          policy,
        },
        (err: unknown, result: unknown) => {
          if (err) return reject(err);
          resolve(result as never);
        },
      );
    });

    return {
      tmpSecretId: data.credentials.tmpSecretId,
      tmpSecretKey: data.credentials.tmpSecretKey,
      sessionToken: data.credentials.sessionToken,
      startTime: data.startTime,
      expiredTime: data.expiredTime,
    };
  }
}

let singleton: CosStorage | null = null;

export function storage(): Storage {
  if (!singleton) singleton = new CosStorage();
  return singleton;
}
