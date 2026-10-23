function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`缺少环境变量 ${name}`);
  return value;
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
};

export function cosAppId(bucket: string): string {
  const parts = bucket.split("-");
  return parts.length > 1 ? parts[parts.length - 1] : "";
}
