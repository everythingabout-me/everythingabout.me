function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`缺少环境变量 ${name}`);
  return value;
}

function positiveNumber(name: string, fallback: number): number {
  const value = Number(process.env[name]);
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

export const env = {
  get databaseUrl() {
    return required("DATABASE_URL");
  },
  get cos() {
    return {
      secretId: required("COS_SECRET_ID"),
      secretKey: required("COS_SECRET_KEY"),
      bucket: required("COS_BUCKET"),
      region: required("COS_REGION"),
      publicBase: process.env.COS_PUBLIC_BASE || "",
    };
  },
  /** 登录态有效期，默认 30 天 */
  get sessionTtlDays() {
    return positiveNumber("SESSION_TTL_DAYS", 30);
  },
  /** 正式环境默认给 cookie 加 Secure；反代没上 HTTPS 时可用 COOKIE_SECURE=false 临时关掉 */
  get cookieSecure() {
    const raw = process.env.COOKIE_SECURE;
    if (raw === "true") return true;
    if (raw === "false") return false;
    return process.env.NODE_ENV === "production";
  },
  /** 设为 false 可关闭注册；不设置时默认开放 */
  get registrationOpen() {
    return process.env.ALLOW_REGISTRATION !== "false";
  },
  /**
   * 腾讯云 SES（注册验证码）。
   * SecretId/SecretKey 不单独填时复用 COS 的密钥；都为空则视为未配置。
   */
  get ses() {
    return {
      secretId: process.env.SES_SECRET_ID || process.env.COS_SECRET_ID || "",
      secretKey: process.env.SES_SECRET_KEY || process.env.COS_SECRET_KEY || "",
      region: process.env.SES_REGION || "ap-guangzhou",
      fromEmailAddress: process.env.SES_FROM_EMAIL_ADDRESS || "",
      templateId: Number(process.env.SES_TEMPLATE_ID || 0),
      templateVar: process.env.SES_TEMPLATE_VAR || "code",
      subject: process.env.SES_SUBJECT || "Everything About Me 注册验证码",
      replyTo: process.env.SES_REPLY_TO_ADDRESS || "",
    };
  },
};

export function cosAppId(bucket: string): string {
  const parts = bucket.split("-");
  return parts.length > 1 ? parts[parts.length - 1] : "";
}
