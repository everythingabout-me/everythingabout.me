type BrandMarkProps = {
  size?: number;
  className?: string;
};

/** The circle mark. Decorative: the adjacent wordmark carries the name. */
export function BrandMark({ size = 21, className }: BrandMarkProps) {
  return (
    <img
      className={className}
      src="/logo.svg"
      alt=""
      width={size}
      height={size}
      draggable={false}
      decoding="async"
    />
  );
}
