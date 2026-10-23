import { ses } from "tencentcloud-sdk-nodejs-ses";
import { HttpError } from "./errors";

/** 发件人/模板/密钥都齐了才算配置好 */
function sesConfig() {
  const secretId = process.env.SES_SECRET_ID || process.env.COS_SECRET_ID || "";
  const secretKey = process.env.SES_SECRET_KEY || process.env.COS_SECRET_KEY || "";
  const fromEmailAddress = process.env.SES_FROM_EMAIL_ADDRESS || "";
  const templateId = Number(process.env.SES_TEMPLATE_ID || 0);
  if (!secretId || !secretKey || !fromEmailAddress) return null;
  if (!Number.isFinite(templateId) || templateId <= 0) return null;

  return {
    secretId,
    secretKey,
    fromEmailAddress,
    templateId,
    region: process.env.SES_REGION || "ap-guangzhou",
    templateVar: process.env.SES_TEMPLATE_VAR || "code",
    subject: process.env.SES_SUBJECT || "Everything About Me 验证码",
    replyTo: process.env.SES_REPLY_TO_ADDRESS || "",
  };
}

export function emailDeliveryConfigured(): boolean {
  return sesConfig() !== null;
}

/**
 * 发邮箱验证码（登录 / 注册 / 改邮箱共用）。
 * - 配好 SES：真实发信
 * - 没配且是开发环境：打印到服务端日志，并把验证码返回给页面自动填入
 * - 没配且是生产环境：明确报错，不静默
 */
export async function sendVerificationCode(
  to: string,
  code: string,
): Promise<{ devCode?: string }> {
  const cfg = sesConfig();

  if (!cfg) {
    if (process.env.NODE_ENV === "production") {
      throw new HttpError(503, "邮件服务未配置，暂时无法发送验证码");
    }
    console.info(`[email:dev] 验证码 ${code} -> ${to}`);
    return { devCode: code };
  }

  const client = new ses.v20201002.Client({
    credential: { secretId: cfg.secretId, secretKey: cfg.secretKey },
    region: cfg.region,
    profile: {
      httpProfile: {
        endpoint: "ses.tencentcloudapi.com",
        reqTimeout: 10,
      },
    },
  });

  try {
    await client.SendEmail({
      FromEmailAddress: cfg.fromEmailAddress,
      Subject: cfg.subject,
      Destination: [to],
      Template: {
        TemplateID: cfg.templateId,
        TemplateData: JSON.stringify({ [cfg.templateVar]: code }),
      },
      // 1 = 触发类邮件（验证码这类即时邮件）
      TriggerType: 1,
      ...(cfg.replyTo ? { ReplyToAddresses: cfg.replyTo } : {}),
    });
  } catch (error) {
    console.error("[email] SES SendEmail failed", error);
    const message = error instanceof Error ? error.message : String(error);
    throw new HttpError(502, `验证码邮件发送失败：${message}`);
  }

  return {};
}
