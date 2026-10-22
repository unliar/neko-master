import type { Metadata, Viewport } from "next";
import Script from "next/script";
import { Geist, Geist_Mono } from "next/font/google";
import "../globals.css";
import { ThemeProvider } from "@/components/providers/theme-provider";
import { QueryProvider } from "@/components/providers/query-provider";
import { AuthProvider } from "@/lib/auth";
import { AuthGuard } from "@/components/features/auth/auth-guard";
import { NextIntlClientProvider } from "next-intl";
import { getMessages, setRequestLocale } from "next-intl/server";
import { notFound } from "next/navigation";
import { routing } from "@/i18n/routing";
import { ServiceWorkerRegister } from "@/components/common";
import { Toaster } from "@/components/ui/sonner";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  title: "Neko Master",
  description: "Modern traffic analytics for edge gateways",
  icons: {
    icon: "/logo.png",
    shortcut: "/logo.png",
    apple: "/icons/icon-192x192.png",
  },
  appleWebApp: {
    capable: true,
    statusBarStyle: "black-translucent",
    title: "Neko Master",
  },
  applicationName: "Neko Master",
};

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#F5F7FA" },
    { media: "(prefers-color-scheme: dark)", color: "#0B0F19" },
  ],
  width: "device-width",
  initialScale: 1,
  maximumScale: 1,
  userScalable: false,
};

// 静态导出（output: 'export'）要求为动态路由段 /[locale] 枚举所有取值，
// 否则构建时会报 "missing generateStaticParams"。这里按 i18n 配置的
// locales 逐个预渲染，产出 /zh/ 与 /en/ 两套静态页面。
export function generateStaticParams() {
  return routing.locales.map((locale) => ({ locale }));
}

export default async function RootLayout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  if (!routing.locales.includes(locale as (typeof routing.locales)[number])) notFound();

  // next-intl 静态渲染要求：必须在调用 getMessages() 等 API 之前，
  // 把当前 locale 分发给本次渲染的所有 Server Components，
  // 否则静态导出时会因读不到请求级 locale 而退回动态渲染。
  setRequestLocale(locale);

  const messages = await getMessages();

  return (
    <html lang={locale} suppressHydrationWarning>
      <head>
        <link rel="manifest" href="/manifest.webmanifest" />
        <link rel="apple-touch-icon" href="/icons/icon-192x192.png" />

        <meta name="mobile-web-app-capable" content="yes" />
        <meta name="apple-mobile-web-app-capable" content="yes" />
        <meta
          name="apple-mobile-web-app-status-bar-style"
          content="black-translucent"
        />
        <meta name="apple-mobile-web-app-title" content="Neko Master" />
        {/*
          Load runtime config (real external ports/URLs written by
          docker-start.sh) BEFORE the app bundle, so the WebSocket client reads
          window.__RUNTIME_CONFIG__.WS_PORT instead of racing and falling back
          to the build-time default. See issue #61.
        */}
        <Script src="/runtime-config.js" strategy="beforeInteractive" />
      </head>
      <body
        className={`${geistSans.variable} ${geistMono.variable} antialiased`}>
        <ThemeProvider attribute="class" defaultTheme="system" enableSystem>
          <QueryProvider>
            <AuthProvider>
              <NextIntlClientProvider messages={messages}>
                <AuthGuard />
                {children}
              </NextIntlClientProvider>
            </AuthProvider>
          </QueryProvider>
        </ThemeProvider>
        <ServiceWorkerRegister />
        <Toaster position="top-center" />
      </body>
    </html>
  );
}
