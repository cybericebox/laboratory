    (function(){
      var root=document.documentElement,buttons=document.querySelectorAll('[data-choice]');
      var system=window.matchMedia('(prefers-color-scheme: dark)'),choice='system';
      try{var saved=localStorage.getItem('lab-probe-theme');if(saved==='light'||saved==='dark')choice=saved}catch(_){}
      function apply(){
        root.dataset.theme=choice==='system'?(system.matches?'dark':'light'):choice;
        buttons.forEach(function(button){button.setAttribute('aria-checked',String(button.dataset.choice===choice))});
      }
      buttons.forEach(function(button){button.addEventListener('click',function(){
        choice=button.dataset.choice;
        try{localStorage.setItem('lab-probe-theme',choice)}catch(_){}
        apply();
      })});
      system.addEventListener('change',function(){if(choice==='system')apply()});
      apply();
    })();
