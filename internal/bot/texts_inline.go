package bot

// These are the hard-coded user-facing strings the Python handlers embedded
// directly in code (as opposed to the Texts/*.txt templates served by
// internal/texts). They are reproduced byte-for-byte from the Python source so
// the bot's replies are identical after cutover.
const (
	// handler_templates.py / send_msg_template
	txtSessionExpired     = "نشست شما منقضی شده. لطفاً دوباره از لینک ناشناس استفاده کنید."
	txtContactChangedLink = "مخاطبت لینکش رو عوض کرده. باید از نو پیام بفرستی"
	txtSendingToAnother   = "در حال ارسال پیام به یکی دیگه بودی. کنسلش کردم. دوباره ریپلای بزن به فردی که میخواستی"
	txtBlockedYou         = "این کاربر بلاکت کرده خخ"
	txtNotifNewAnswer     = "جواب جدید:"

	// shared link / contact errors (start.py, myhelpers.py, handler_templates.py)
	txtLinkDeletedOrChanged = "مخاطبت این لینک رو پاک یا عوض کرده. با لینک جدید بهش پیام بده"

	// start.py
	txtWrongLink        = "لینکت اشتباهه?"
	txtNotBlockedAnyway = "این یوزر اصن برات بلاک نبود"
	txtUserBanned       = "این کاربر از بات بن شده اصن"

	// is_answer / is_reply_to_channel
	txtWrongReply              = "اگه میخوای جواب بدی، باید خود پیام ناشناس رو ریپلای کنی. اونی که زیرش دکمه های شیشه ای هست"
	txtChannelPrivateNotAdded  = "چنل مد نظرت پرایوته و بات بهش اضافه نشده"
	txtChannelNoLinkInBioOrPin = "چنل مدنظرت لینک ناشناسی توی بایو یا پیام پین شده ش نذاشته"
	txtNoAnonLinkFound         = "لینک ناشناسی توی بایو (یا پیامِ پین شده) ی چنل مدنظرت پیدا نکردم"

	// other_msgs.py / templates
	txtNotUnderstood   = "متوجه نشدم. اگه کمک میخوای از /help استفاده کن"
	txtNothingToCancel = "چیزی واسه کنسل کردن وجود نداره"

	// cancel handlers (start.py)
	txtCancelledMidSend = "وسط ارسال پیام بودی. کنسلش کردم. دوباره بفرست"
	txtCancelledAll     = "هرچی که بود کنسل شد"
	txtCancelGone       = "چشم بهم بزنی این پیام نیس👋"

	// media (start.py handle_media)
	txtTooLate      = "دیر شد. توی یه پیام جدید بفرست"
	txtMediaUseThis = "<blockquote>برای جواب دادن و اینجور چیزا، ازین پیام استفاده کن</blockquote>"

	// handle_target_send (decorators.py)
	txtChannelNotAddedRetry = "چنلی که ازش ریپلای کردی بات رو به خودش اضافه نکرده. اول باید از ادمینش بخوای که اینکارو کنه." +
		"\n\nدوباره پیامتو بفرست."
	txtBotBlockedByContact   = "مخاطبت بات رو بلاک کرده"
	txtMaybeBlockedOrCleared = "مخاطبت احتمالا بات رو بلاک کرده. ممکنم هست بخاطر کلیر هیستوری، پیام مدنظرت پاک شده باشه. میتونی لینکشو تست کنی تا ببینی میشه پیام فرستاد یا نه.\n\n" +
		"<blockquote>بخاطر ویژگیِ تلگرام، تا پیام معمولی نفرستم بهش نمیتونم مطمئن شم بات رو بلاک کرده یا نه</blockquote>"

	// seen callback
	txtSeenSent        = "این پیامت سین شد"
	txtToldThemSeen    = "بهش گفتم سین زدی"
	txtAlreadySeenOnce = "یبار سین زدم"

	// block / unblock callbacks
	txtBlockSuccess   = "با موفقیت بلاک شد."
	txtBlockSelf      = "یه تراپی برو💀👍"
	txtAlreadyBlocked = "همین الانش بلاک هست"
	txtUnblockSuccess = "با موفقیت آنبلاک شد."
	txtUnblockSelf    = "خوبه پس تراپی جواب داد🥹"
	txtNotBlockedNow  = "همین الانش بلاک نیس"

	// report callbacks
	txtReportConfirm   = "آیا واقعا میخواهید این پیام را گزارش کنید؟"
	txtReportCancelled = "لغو شد"

	// txtBannedNotice is sent to a user an admin bans from a report. Only the ban
	// is announced: the user finds out anyway when the bot stops answering, so
	// saying it plainly is clearer, while a merely-registered report stays silent.
	txtBannedNotice = "🚫 دسترسی شما به این بات بسته شد.\n\n" +
		"دلیلش گزارش‌های کاربران دیگه از پیام‌های شما بوده. اگر فکر می‌کنید اشتباهی پیش اومده، از طریق کانال با ما در تماس باشید."
	txtReportedFromTargetChat = "این پیام از چت گیرنده کپی شد. ممکنه تهش تگ دلخواه داشته باشه"
	btnReportYes              = "✅ آره ریپورتش کن"
	btnReportNo               = "❌ نهههه"

	// delete callback
	txtDeletedForThem = "پاکش کردم براش😮‍💨"

	// warning + deletion
	btnDelete       = "پاکش کننن"
	txtSentToThem   = "فرستادم بهش."
	txtWarnBugInner = "⚠️به هیچ پیام فوروارد شده ای ریپلای نزن. چرایی: /bug⚠️"

	// keyboard extras
	btnDonation = "با دونیشن از ما حمایت کنید ♥"

	// answer callback prompt
	txtAnswerPrompt = "جوابت به این پیام رو بفرست\n\n" +
		"<blockquote>میدونستی ازین به بعد نیاز نیست حتما از دکمه ی ارسال جواب استفاده کنی. فقط کافیه پیام رو ریپلای کنی و جوابتو بهش بنویسی، مث یه چت معمولی :)</blockquote>"

	// start connect prompt fragments
	txtConnectSelf = "میخوای با خودت صحبت کنی؟ :) عب نداره راحت باش."
	// The connect prompt doubles as a reusable anchor (its cancel button carries
	// the link id — see connectCancelMarkup), so it says so. Only shown when the
	// id actually fit on the button.
	txtConnectAnchorHint = "<i>برای پیام بعدی هم کافیه همین پیام رو ریپلای کنی؛ دیگه لازم نیست از لینک بری.</i>"
	txtConnectBody       = "<blockquote>میدونستی میتونی بدون استفاده از لینک، فقط با ریپلای کردن به کانال پیام بدی؟ منوی قابلیت ها و تنظیمات رو چک کن ؛)</blockquote>"

	// unblock-me-again button on /start UNBLOCK reply
	btnBlockAgain = "پشیمون شدم دوباره بلاکش کن خخ"

	// Telegram-imposed flood wait (HTTP 429). Shown instead of an error code: this
	// is the bot being busy, not a fault the user can do anything about but retry.
	txtTooManyRequests = "الان یه کم شلوغه و تلگرام جلوی ارسال رو گرفته 😮‍💨\n" +
		"چند لحظه صبر کن و دوباره بفرست."

	// outbound rate limit (Go-only hardening; generous threshold, see allowSend)
	txtTooFast = "یه کم آروم‌تر! بیش از حد پیام فرستادی، چند لحظه صبر کن و دوباره امتحان کن."

	// prep / @prep_function (decorators.py)
	txtInitNoCID      = "مشکلی در ساخت لینک ناشناس بوجود اومد. دوباره تلاش کن و اگه موفق نشدی، قبل از استفاده از بات با پشتیبانی تماس بگیر"
	txtDBProblem      = "مشکلی برای دیتابیس به وجود آمده. به پشتیبانی خبردادیم، لطفا صبر کنید و دوباره تلاش کنید"
	txtNetworkProblem = "به مشکل اینترنتی برخوردیم. لطفاً دوباره تلاش کنید."
)
