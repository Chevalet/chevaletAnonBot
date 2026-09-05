package bot

// Hard-coded user-facing strings from modules/settings.py and
// modules/my_links.py (the templated ones live in Texts/settings/* and
// Texts/mylinks.txt and are served by internal/texts). Reproduced byte-for-byte.
const (
	// settings.py
	txtSettingsTagPlaceholder  = "[تگی ثبت نکردی]"
	txtWPPDefault              = "حالت پیشفرض"
	txtWPPDisabled             = "غیرفعال"
	txtStateActive             = "فعال"
	txtStateInactive           = "غیرفعال"
	txtWPPAnswerActivate       = "پیشنمایش به حالتِ پیشفرض تبدیل شد"
	txtWPPAnswerDeactivate     = "پیشنمایش غیرفعال شد"
	txtWarningAnswerActivate   = "اخطار فعال شد"
	txtWarningAnswerDeactivate = "اخطار غیرفعال شد"
	txtSeenAnswerActivate      = "آپشن سین زدن فعال شد"
	txtSeenAnswerDeactivate    = "آپشن سین زدن غیرفعال شد"
	txtTagRemoved              = "تگ دلخواهت با موفقیت پاک شد"
	txtUnblockAllDone          = "همه با موفقیت آنبلاک شدن"
	txtUnblockAllConfirm       = "اگه میخوای همه رو آنبلاک کنی و مطمئنی، دکمه ی زیر رو بزن"
	btnUnblockAllSure          = "مطمئنم، بزن همه رو آنبلاک کن"
	txtUnblockMeIntro          = "این لینک رو بفرس به یکی که بلاکت کرده. وقتی که بزنه روش آنبلاک میشی:"
	txtSettingsCancelAll       = "در حال تغییر تنظیماتت بودی پس کنسلش کردم. دوباره امتحان کن"
	txtCancelledAllGeneric     = "هرچی که بود کنسل شد"

	// anonymous nickname (optional per-sender signature)
	txtAnonNoneYet      = "[هنوز نساختی]"
	txtAnonEnabled      = "نام مستعار روشن شد ✅"
	txtAnonDisabled     = "نام مستعار خاموش شد. پیام‌هات دوباره کاملاً ناشناسن"
	txtAnonSetPrompt    = "اسمِ مستعارِ ناشناستو توی پیام بعدی بفرست:"
	txtAnonEmojiPrompt  = "خب حالا ایموجی‌شو بفرست.\nاگه نمی‌خوای ایموجی داشته باشه، دکمه‌ی «بدون ایموجی» رو بزن."
	txtAnonSaved        = "ثبت شد ✅ نام مستعارت این شد:\n%s\n\nمیتونی لینک خودتو تست کنی تا ببینی چجوری نشون داده میشه :)"
	txtAnonRemoved      = "نام مستعارت پاک شد و قابلیت خاموش شد. حالا پیام‌هات کاملاً ناشناسن"
	txtAnonNameTooLong  = "اسم مستعار نباید بیشتر از %dتا حرف باشه. دوباره امتحان کن.\nنکته: قالب‌بندی یه مقدار به تعداد حروفت اضافه میکنه"
	txtAnonEmojiTooLong = "ایموجی نباید بیشتر از %dتا کاراکتر باشه. دوباره امتحان کن"
	btnAnonActivate     = "✅ فعالسازی"
	btnAnonDeactivate   = "❌ غیرفعالسازی"

	// display-name syncing (name_sync)
	txtNameSyncAnswerActivate   = "از این به بعد نام نمایشیت با اسم اکانتت هماهنگ می‌مونه ✅"
	txtNameSyncAnswerDeactivate = "هم‌گام‌سازی خاموش شد. نام نمایشیت همینی که هست می‌مونه"
	// Shown after a MANUAL rename that switched syncing off, so the user is not
	// left wondering why a setting they never touched changed.
	txtNameSyncTurnedOffByRename = "\n\n<blockquote>🔄 چون اسمو دستی ثبت کردی، هم‌گام‌سازی خودکارِ نام با اکانتت خاموش شد تا این اسم از بین نره. هر وقت خواستی از تنظیمات دوباره روشنش کن.</blockquote>"

	// my_links.py
	txtMyLinksPromo         = "<blockquote>راستی یادت نره به کانالمون سر بزنی:\n@chevalet_studio</blockquote>"
	txtAddedNewLink         = "added a new link"
	txtRmConfirmTail        = "\n\nمطمئنی از حذفش؟ قابل برگشت نیست"
	btnRmSure               = "✅ آره مطمئنم"
	btnRmNo                 = "نههه پاک نکن"
	txtRmDeletedRegen       = "با موفقیت حذف شد ولی چون فقط یک لینک داشتی، بجاش یکی دیگه تولید شد"
	txtRmDeleted            = "با موفقیت حذف شد."
	txtRmCancelled          = "کنسل شد"
	txtChChooseTail         = "\n\nانتخاب کن"
	txtCidCharErr           = "فقط حروف کوچیک و بزرگ انگلیسی، اعداد، آندرلاین و خط تیره مجازه. دوباره امتحان کن"
	txtCidTaken             = "این آیدی برداشته شده. آیدی دیگه ای بفرس"
	txtCidChanged           = "با موفقیت تغییر یافت"
	txtCidIntegrity         = "ظاهرا یکی زودتر این آیدی رو برداشت. دوباره امتحان کن"
	txtChangingCidCancelled = "در حال تغییر آیدی بودی پس کنسلش کردم. دوباره بفرست"
)
