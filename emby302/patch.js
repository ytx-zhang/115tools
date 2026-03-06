; (function () {
    // 1. 跨域修复
    const fix = () => {
        const p = window.defined?.["modules/htmlvideoplayer/plugin.js"]?.default?.prototype;
        if (p) return p.getCrossOriginValue = () => null;
        setTimeout(fix, 100);
    };
    fix();

    let lastPause = 0;
    var oldSetSrc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'src').set;
    Object.defineProperty(HTMLMediaElement.prototype, 'src', {
        set: function (url) {
            this._isCDN = url && url.includes('is302=1');

            if (this._isCDN) {
                console.log("[302] 超时监控已开启");
            }
            oldSetSrc.call(this, url);
        }
    });
    document.addEventListener('pause', (e) => lastPause = Date.now(), true);

    document.addEventListener('play', e => {
        const v = e.target;
        if (v.tagName !== 'VIDEO' || !v._isCDN) return;
        if (lastPause && (Date.now() - lastPause > 5 * 60 * 1000)) {
            console.warn("[302] 暂停超5分钟,触发重载");
            lastPause = 0
            const currentTime = v.currentTime;
            v.src = v.src;
            v.addEventListener('loadedmetadata', () => {
                v.currentTime = currentTime;
            }, { once: true });
        }
    }, true);
})();