import { LandingPage } from "@/components/landing/landing-page";
import { getCopy } from "@/config/copy";

export default function Page() {
  return <LandingPage locale="zh-CN" copy={getCopy("zh-CN")} />;
}
